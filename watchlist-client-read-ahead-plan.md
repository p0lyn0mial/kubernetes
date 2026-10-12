# WatchList client read-ahead plan

Status: design and investigation record, updated 2026-10-12. Keep Go's
transport-owned automatic gzip decompression. This supersedes the proposal to
queue compressed bytes and implement client-go gzip handling. The paged ring
has been implemented and tested; the findings below distinguish local
microbenchmarks, user-reported workload results, and remaining hypotheses.
Performance targets are not achieved results unless explicitly identified.

## Objective and chosen boundary

Make client-go consume the response body as quickly as possible so the server
can send more data without waiting for client-side object processing. The
primary outcome is less server-side write blocking and lower server WatchList
completion latency. An independent processor frames and decodes the preceding
bytes concurrently. Preserve event order and bound memory with resource-specific
byte budgets.

Faster reflector/informer synchronization is a secondary outcome, not a
requirement for demonstrating the intended server-side benefit. Measure it
and its backlog so earlier server completion is not presented as earlier
application readiness. Set acceptable downstream latency and memory limits
before selecting a configuration.

The receiver reads directly from the existing `resp.Body`. When Go's transport
negotiates gzip automatically, these reads include decompression. The queue
therefore contains decompressed, unparsed bytes. The processor owns framing,
WatchEvent decoding, and embedded-object decoding; the reflector owns stores.
This is the meaning of "raw stream" in this design: raw to Kubernetes decoding,
while retaining Go's normal HTTP and gzip handling.

Implemented baseline (the ordered processor follow-up below is not implemented):

```text
Receiver goroutine:
  resp.Body.Read (transport + automatic gzip) -> reusable block -> bounded FIFO
                                                                       |
Processor / existing StreamWatcher goroutine:          bytes -> framing
                                                            -> envelope decode
                                                            -> object decode
                                                                       |
                                                        unbuffered ResultChan
                                                                       |
Reflector goroutine:                                             receive -> store
```

Start processing as soon as bytes are available. Do not wait for a complete
snapshot, frame, or full buffer before handing data off. Watches can remain
open indefinitely.

Automatic gzip is lazy work performed during body reads; the transport does
not make decompression free or move it to an independent CPU. This design
overlaps reading/decompression with parsing and object processing. It does
not promise receiving compressed wire bytes independently of decompression.

## Evidence and scope of the change

The first prototype, committed as `3a3b83b3a64`, already queues `resp.Body`
before framing. Under automatic gzip this is the desired boundary. Retain that
architecture and improve its overhead, sizing, and lifecycle.

The earlier compressed-queue revision introduced request-side encoding
negotiation, manual gzip handling, response-header normalization, and extra
error-path behavior. Those are no longer implementation goals. When aligning
an implementation with this plan, remove only those feature-specific changes;
preserve independent fixes and measured queue improvements.

Existing investigations provide useful historical context:

- [Encoding and backpressure](watchlist-encoding-client-backpressure.md):
  the faster HTTP benchmark decodes envelopes but leaves embedded Pods raw.
- [Profiling plan](watchlist-client-profiling-plan.md): distinguishes transfer,
  typed decoding, retention, and informer synchronization.
- [Remote profiling](watchlist-remote-repro-status.md): reports typed decoding,
  allocation/GC, gzip, envelope copies, and CPU throttling.

Verify the current benchmark and compiled dependency revisions before reusing
those findings. They motivate the experiment but do not establish a speedup.

The implementation remains opt-in for watches with `sendInitialEvents=true`.
Zero budget preserves the existing path without a new goroutine or queue.
Keep the queue for the selected stream's lifetime, including after the initial
bookmark. Keep `ResultChan` unbuffered initially. Do not change the server,
wire protocol, generated clients, or apimachinery APIs for the queue work.

## Findings from implementation and workload testing

### HTTP WatchList is a faster consumption path with less downstream work

Inspection of
[request-benchmark HTTP mode](https://github.com/kubernetes/perf-tests/blob/master/util-images/request-benchmark/mode_http.go)
confirmed that `consumeWatchList` uses the normal HTTP response body, frames
the stream, and decodes `metav1.WatchEvent` envelopes. It leaves embedded
resource objects in `event.Object.Raw`, discards them, and returns at the
initial-events-end bookmark. It does not decode typed Pods, retain a snapshot,
or wait for informer synchronization.

With automatic gzip enabled, both this HTTP mode and the read-ahead receiver
consume decompressed body bytes. The user's approximately 40 MiB network
payload is therefore not the quantity held in the read-ahead queue. The trace
below reports approximately 967 MiB of expanded bytes.

The HTTP result supports the feasibility of faster reading/decompression and
envelope consumption under its tested conditions. It does not show that typed
decoding and retention can run concurrently at no cost. The informer adds
typed decoding, event delivery, temporary-store insertion, and retention/GC.
These can slow intake through both a full buffer and shared CPU/GC pressure.
Final Replace/indexing also affects informer readiness, but occurs after the
initial bookmark and should not be charged directly to earlier server writes.

### Increasing the buffer from 1 to 4 MiB did not improve the reported result

The user tested the paged implementation with
`ExperimentalWatchListReadAheadMaxBytes=1048576`, then 4194304, and reported no
latency improvement from that increase. No exact paired latency distribution
or complete configuration was supplied for this comparison. This rules out
the suggestion that the configured budget was zero; it does not establish
that arbitrary larger buffers would have no effect.

### Early receiver trace: capacity waits are substantial

The user supplied this trace while the workload test was still running:

```text
WatchList read-ahead receiver trace
elapsed=7.898s bodyReadTime=4.728s spaceWaitTime=3.154s
bodyReadCalls=46618 spaceWaitCount=9505 bytesRead=1014430847
budgetBytes=4194304
```

| Derived value | Result |
| --- | ---: |
| Decompressed bytes received | 967.44 MiB |
| Time inside body reads / elapsed | 59.86% |
| Time in capacity waits / elapsed | 39.93% |
| Elapsed minus the two reported timers | approximately 16 ms |
| Bytes / elapsed | 122.49 MiB/s |
| Bytes / bodyReadTime | 204.62 MiB/s |
| Average body read size | 21.25 KiB |
| Average capacity wait | 331.83 microseconds |

This demonstrates repeated downstream backpressure at the receive queue. The
receiver cannot keep reading while the processor has not released capacity.
It explains why a faster queue alone need not bring informer-driven server
latency down to the HTTP benchmark's latency.

Interpret the timers carefully:

- This is an early observation, not a verified final request/bookmark duration
  or a distribution across runs.
- `bodyReadTime` includes transport waiting and gzip, not just network or CPU.
- Capacity-wait wall time can include scheduling delay around a wakeup.
- The 16 ms remainder is not all queue overhead: consumer copying, wakeup
  effects, and CPU/GC competition can be accounted for elsewhere.
- 204.62 MiB/s is throughput normalized by observed body-read time, not a
  proven sustainable no-backpressure rate. Removing waits can change both
  server and client scheduling. 4.728 s is not a predicted completion time.

As a sizing illustration, assume the observed rates stay constant and the
4 MiB queue is approximately full at the trace boundary. Approximating consumer
rate by `(bytesRead-budgetBytes)/elapsed` gives roughly 391 MiB of backlog
at the hypothetical body-read-only completion time. This is an estimate, not
measured occupancy or a recommended default. It shows why another few MiB may
not meaningfully change a large-stream result.

A single-stream 512 MiB diagnostic run could test the effect of substantially
reducing capacity stalls, if memory permits. It has not been run as part of
this investigation. Do not turn that diagnostic size into a default or apply
it across many informers without an aggregate memory budget.

### Local ring benchmarks do not establish a general speed advantage

The [receiver design's benchmark record](watchlist-byte-receiver-design.md#local-microbenchmark-results)
contains the commands and results. On the local Apple M4 Pro, the ring lost to
the channel control for full-size body reads with a 1 MiB budget, but won with
4 KiB-limited body reads. These tests copy an approximately 1.06 MiB memory
payload and exclude HTTP, gzip, typed decoding, and reflector work.

The fixed ring eventually allocates each page as its cursor traverses the
configured capacity, even with a shallow backlog. The channel control can
reuse a smaller set of blocks when consumption keeps up. Therefore lazy page
allocation is not the same as allocation proportional to peak backlog.

The next priority is processor throughput, keeping the current transport,
receiver implementation, and budget fixed for comparisons. Separate typed
decoding/allocation time from decoded-event delivery waits and reflector work.
The trace establishes capacity backpressure, but does not yet identify the
dominant downstream function or prove a specific optimization's benefit.

### Inspiration from the apiserver's etcd receive path

Inspection of this checkout found an existing receive/process separation in
[the etcd storage watcher](staging/src/k8s.io/apiserver/pkg/storage/etcd3/watcher.go):

- gRPC receives and decodes the etcd response envelope, but Kubernetes objects
  remain serialized in `KV.Value`. This is not raw socket-byte reception.
- [parseKV/parseEvent](staging/src/k8s.io/apiserver/pkg/storage/etcd3/event.go)
  retain the value slices without another object-payload copy at that step.
  `queueEvent` passes these lightweight events through a 100-entry input
  channel. Storage transformation/decryption and object decoding happen later
  in `prepareObjs`, not in the receive loop.
- With `ConcurrentWatchObjectDecode` enabled, `concurrentProcessEvents` allows
  up to 10 outstanding transformations. It schedules a result channel for each
  event in input order; its collector waits on those channels in that order.
  Completion can be out of order, but delivery cannot. The output channel also
  holds 100 events. These are event-count bounds, not byte budgets.
- The feature declaration describes avoiding watch-cache starvation with
  conversion webhooks. This is architectural precedent, not evidence that
  parallel CPU-bound Pod decoding will improve our client workload. Check
  effective feature configuration before attributing a running server's
  performance to this path.

Distinguish two upstream etcd client paths:

| Path | Receive/buffering behavior in this checkout |
| --- | --- |
| Live watch | `serveWatchClient` receives gRPC messages; `serveSubstream` retains undelivered responses in a growing slice with a TODO to limit it. There is no configured backlog cap here. |
| Initial RangeStream, when selected and supported | `GetStream` receives in a goroutine through a one-response buffered channel; `syncStreamRecursive` queues each serialized KV into the storage watcher's input channel. It does not use the live-watch backlog. |

Sources: [etcd watch client](vendor/go.etcd.io/etcd/client/v3/watch.go) and
[etcd KV client](vendor/go.etcd.io/etcd/client/v3/kv.go). The initial sync can
also use paginated Get; do not assume RangeStream is active in a deployment.

Borrow the separation of serialized values from typed decoding and the ordered
collector. Do not copy the unbounded backlog, the constants 100/10, or the
per-event goroutine/result-channel allocations. The proposed client processor
uses persistent workers and bounded reusable result slots. No server, etcd,
or vendor changes are part of this plan.

## Preserve transport behavior

Install the reader in `Request.newStreamWatcher` after successful serializer
negotiation and before `framer.NewFrameReader`. Leave request construction,
compression negotiation, response processing, authentication, retries, and
shared transport configuration as they are without this feature.

Specifically:

- Do not inject `Accept-Encoding: gzip`. An explicit gzip request prevents
  standard Go transport automatic decompression.
- Do not change `DisableCompression`, introduce a custom gzip reader, or
  normalize compression headers as part of read-ahead.
- Do not add compression-specific handling for Status/error responses.
- Pass through the bytes and errors supplied by the existing response body.
- Let transport behavior handle gzip members, checksums, and decompression.
- Preserve the caller's custom HTTP client, RoundTripper, and headers.

With compression disabled or an identity response, the same queue buffers
uncompressed bytes. A custom transport must continue to supply the body
representation the existing watch decoder expects. Read-ahead does not repair
a custom transport/caller combination that delivers compressed bytes directly
to the decoder. Record actual response/decompression behavior in benchmarks.

The integration point is
[request.go](staging/src/k8s.io/client-go/rest/request.go).
The processor continues through the existing
[streaming decoder](staging/src/k8s.io/apimachinery/pkg/runtime/serializer/streaming/streaming.go),
[watch decoder](staging/src/k8s.io/client-go/rest/watch/decoder.go), and
[StreamWatcher](staging/src/k8s.io/apimachinery/pkg/watch/streamwatcher.go).
The [reflector](staging/src/k8s.io/client-go/tools/cache/reflector.go) keeps its
existing bookmark and store semantics.

## Fast receiver and processor handoff

The implemented candidate is a paged single-producer/single-consumer byte
ring, specified in [the receiver design](watchlist-byte-receiver-design.md).
It publishes byte cursors instead of handing every read through ready/free
channels. Notification channels are used only for parking on empty/full.
Short reads leave the unwritten page suffix available to the producer, avoiding
the current block queue's partial-block capacity loss. Keep the existing channel
implementation as a benchmark control and adopt the ring only with evidence.

The following principles apply to both candidates; the linked specification
defines the ring's hot paths, cursor ordering, lost-wakeup prevention, terminal
errors, wraparound, and cleanup protocol.

Evolve the private helper in
`staging/src/k8s.io/client-go/rest/watch_read_ahead.go`.

The receiver loop is:

```text
reserve writable bytes -> resp.Body.Read(bytes) -> publish available bytes -> repeat
```

1. Read directly into reusable owned storage. Publish a byte cursor in the ring
   or a block descriptor in the control; do not copy into another queue buffer.
2. Use one producer and one consumer. Compare the paged byte ring with the
   ready/free-channel control at the same payload budget and workload. Account
   for both synchronization cost and usable capacity under short reads.
3. Reuse buffers so steady-state operation has no per-read heap allocations.
   Allocate blocks lazily up to the configured limit; measure startup costs.
4. Publish each nonempty read promptly. Do not use `ReadFull`, wait for a full
   batch, or add a batching timer. Sparse live events and bookmarks must not
   wait for more traffic.
5. Keep framing, object inspection, callbacks, and per-event logging off the
   receiver. Gzip performed inside `resp.Body.Read` is expected.
6. Keep statistics local and aggregate outside the hot path. Avoid shared
   atomic counters on every byte/read unless measurement requires them.

Use explicit ownership internally: the producer cannot overwrite published
bytes until the processor releases them. The processor owns its current cursor.
The existing framers consume an `io.ReadCloser` adapter, which copies bytes
into their caller-provided buffers and releases consumed storage for reuse.

This removes an extra receiver-to-queue payload copy; it is not end-to-end
zero-copy. Existing framing, envelope decoding, and typed objects still have
their own copies and allocations.

Remove the first prototype's per-`Read` state mutex only with a correct
shutdown/ownership protocol. External `Close` signals cancellation and closes
the body; it must not race writes to the processor's cursor. Coordinate
cleanup at processor-operation boundaries, outside the per-byte hot path.
The design must cover a processor that has finished decoding and is blocked
sending an event, so there may never be another queue read after cancellation.
If that lifecycle cannot be proved without a lock, retain synchronization until
a measured and correct replacement is available.

A public leased-buffer API is unnecessary for the first patch. Keep ownership
private and expose only what existing consumers need. In particular, adding
`ReadByte` below the framer does not optimize transport gzip: gzip is upstream
of this queue. Add such an adapter method only for a demonstrated downstream
consumer.

Compare internal chunk sizes of 16, 32, 64, and 256 KiB independently from queue
capacity. Start with 64 KiB as a candidate, not a selected default. Underlying
reads can be shorter. The block control can waste capacity; the ring can
publish and reuse partial-page regions but still withholds capacity in an
active consumer span. Record allocated capacity, valid bytes, and withheld
consumed bytes as separate values.

## Memory bounds and resource-specific sizing

Define `MaxBytes` as the sum of capacities of all payload buffers owned by the
helper: queued, producer-owned, consumer-owned, and free/reusable. Reserve a
slot before reading or allocating. A partial final slot ensures allocation
never rounds above the requested budget. A smaller-than-chunk budget is valid.

This bounds read-ahead payload storage, not the entire watch. Channel metadata,
transport/gzip buffers, frame storage, decoded objects, and reflector stores
also consume memory. Avoid global unbounded pools. Free owned storage after
shutdown, including reusable blocks.

The budget is in decompressed bytes under automatic gzip. Historical runs
contained roughly 40.7 MB compressed and 1 GB expanded. A 64 MiB queue cannot
hold that entire initial snapshot while the processor is paused. Do not reuse
compressed-queue sizing assumptions from the previous revision.

Size for temporary differences in stage throughput:

```text
required capacity ≈ peak(bytes produced by Body.Read - bytes released)
                    + unused capacity in active/partially filled blocks
```

For a processing pause of duration D at an intake rate R, R * D is a useful
first estimate of extra backlog. Measure the actual peak and partial-block
waste. If processing is persistently slower, every finite queue eventually
fills; backpressure must then resume. Do not solve that by buffering an entire
large snapshot per informer or by silently growing beyond the cap.

During an initial snapshot, enough spare capacity lets the receiver continue
reading while the processor lags. Draining the HTTP body allows transport
buffers/flow-control capacity to become available for more server output.
Once the queue fills, that separation is lost. Measure the capacity-stall
fraction and server write blocking together to choose a useful budget; a
large configured limit alone is not evidence that the server is unblocked.

The original proposed sweep was disabled, 256 KiB, 1, 4, 16, and 64 MiB.
The reported 1 and 4 MiB runs showed no improvement, and the early trace
established substantial capacity waiting. Further sizing experiments should
measure waits and server latency together, not repeat small increases without
attribution. Locate the smallest useful plateau if one exists. Larger budgets
require evidence of additional benefit. With ten streams at 64 MiB, payload
capacity alone can reach 640 MiB; with 100 streams it can reach 6.25 GiB.
Measure initial synchronization, repeated rounds, and long-lived idle watches.

Reuse `ExperimentalWatchListReadAheadMaxBytes` for the prototype. Its documented
unit is response-body bytes, decompressed when the transport handles gzip.
Extend it to resource policy after measuring the basic implementation.
Conceptual configuration, with provisional names:

```go
type WatchListReadAheadConfig struct {
    DefaultMaxBytes int64
    ByResource      map[schema.GroupVersionResource]int64
}
```

Policy rules:

- Nil policy or a selected zero budget disables read-ahead.
- An exact GVR override takes precedence, including an explicit zero override.
- Missing override/unknown identity uses the default.
- Negative or unrepresentable budgets fail validation before opening a stream.
- Resolve once per request; isolate mutable maps when constructing/copying
  configuration so caller changes cannot race active requests.

Cover `rest.Config`, versioned/unversioned constructors, `RESTClient`,
`Request`, `CopyConfig`, and `AnonymousClientConfig`. Typed clients can use
structured resource metadata. Dynamic and metadata clients use `AbsPath`;
carry their existing GVR separately without altering URLs. Do not guess GVR
from decoded objects or arbitrary URL parsing. Raw-path callers can use the
default policy. Keep resource policy separate from compression configuration.

## Errors, ordering, and shutdown

- Preserve every byte in order. Full queues block; never drop or coalesce data.
- Deliver bytes returned with an error before exposing that terminal error.
- Natural EOF/read errors end production but leave preceding queued data
  consumable. Explicit stop/cancellation may discard pending data.
- Close the underlying body exactly once. Wake a receiver waiting for capacity
  and a processor waiting for bytes; body close interrupts blocked HTTP reads.
- In the channel control, only the producer closes its output channel. In
  the ring, only it closes producerDone after publishing terminal state;
  notification channels remain open. Never race send with external close.
- Do not hold a queue lock while closing the body or awaiting another stage.
  Shutdown must not require another reflector receive.
- Release current, queued, and free blocks after ownership ends. Keeping a
  stopped watch reference must not retain its buffer pool indefinitely.
- Handle already-canceled contexts and cancellation during initialization.
  Remove context callbacks during cleanup.
- Bound repeated `(0, nil)` reads to avoid spinning on a broken body reader.
- Each retry uses a fresh queue. Preserve existing StreamWatcher error
  classification, frame-size checks, and event delivery behavior during Stop.

Queue chunks are not event boundaries. The baseline uses a single decoder;
the proposed concurrent processor must retain a single ordered delivery path
for bookmarks, errors, and initial Adds. Never mark the snapshot complete on
queue drain, HTTP EOF, or a gzip member boundary.

## Benchmark plan and speed targets

### Comparable variants

| Variant | Purpose |
| --- | --- |
| Direct drain of automatically decompressed resp.Body | Intake reference including the same transport/gzip work |
| Existing custom HTTP benchmark | Envelope-only comparison; verify its exact work and timing boundary |
| HTTP loop with typed decoding and discard | Isolate typed decoding and transient allocation |
| Unmodified informer | End-to-end control |
| First committed read-ahead prototype | Queue-overhead and sizing control |
| Paged byte ring versus current ready/free-channel queue | Isolate synchronization overhead and capacity utilization under short reads |
| Revised reader with processor paused, sufficient capacity | Prove independence from framing and typed decoding |
| Revised reader with full processor/reflector active | Main synchronization experiment |
| Same ring/budget with split processor, 1/2/4 decode workers | Measure split-stage overhead and ordered decoding benefit separately from buffer growth |

Keep encoding, compression, resource-version semantics, dataset, selectors,
concurrency, CPU limits, effective GOMAXPROCS, and server cache state matched.
Verify WatchList does not silently fall back to LIST. Record the exact staging
revision compiled into the benchmark; changing this checkout alone does not
change a binary built against released client-go.

Start with a moderate fixture and one client, then the existing approximately
150,000-Pod workload and ten clients. Test under the original CPU quota first;
2/4-CPU runs are separate diagnostics. Previous observations around 1250m make
CPU contention and throttling particularly relevant.

### What fast means here

Proposed intake target: within 5% of a direct drain of the same automatically
decompressed body, using a finite fixture with the processor paused and enough
capacity for its expanded bytes and block waste. This measures overhead added
by queueing. It does not compare against draining compressed wire bytes.

Use a modest fixture for that separation test, not a 1 GB allocation per watch.
For the full dataset, run the processor concurrently with bounded capacity and
measure both full-queue stalls and throughput. Report cold allocation costs
separately from buffer-reuse steady state; include both in the experiment.

Also require:

- Zero steady-state payload/descriptor allocations per read in the queue
  microbenchmark after buffers have reached their working set.
- No framing or Kubernetes decoding on the receiver's stack; gzip is expected.
- No deliberate full-block/batch wait that delays sparse events.
- Reproducible reduction in server write blocking and server WatchList
  completion latency under the matched workload.
- Reflector bookmark/informer-sync latency and retained backlog stay within
  the agreed regression limits; report any improvement separately.

Set the end-to-end success/regression threshold from measured baseline
variability before choosing a configuration. The 5% overhead target is not a
promise of any particular informer speedup.

### Timing and diagnostics

Record request start, response headers, reflector initial bookmark,
destination Replace completion, HasSynced, and shutdown. Record server
WatchList duration separately, correlated by audit ID; it need not include
final flush or client synchronization.

The queue does not parse bookmarks. In deterministic fixtures, use the known
decompressed byte offset through the bookmark to measure intake progress.
That offset is not a compressed wire offset. For real streams, use aggregate
body-read/consumed counts alongside the existing reflector bookmark marker.
EOF is not the usual end of initial events.

Collect queue occupancy, valid bytes versus allocated capacity, producer
capacity wait, consumer input wait, allocations, CPU/GC, live heap/RSS,
throttling, and shutdown time. Body-read duration includes network waiting and
gzip work; it is not pure network time. Do not add overlapping stage wall
times as though they were sequential.

Alternate unprofiled variants, separate cold/warm rounds, and start with
20-30 repetitions for median/spread. Gather more samples for tail claims.
Run CPU/allocation/block profiles separately to quantify instrumentation
overhead. Memory-only GB/s microbenchmarks do not establish HTTP or informer
performance.

## Tests

Use deterministic synchronization instead of sleeps for reader tests.

1. Byte integrity across fragmented reads, short/partial chunks, zero-length
   destinations, rapid reuse, and arbitrary frame/chunk boundaries.
2. EOF/non-EOF errors including data-plus-error; queued bytes precede errors.
3. Budgets below chunk size, nonmultiples, invalid values, and bounded capacity
   with a stalled consumer. No allocation beyond the payload cap.
4. Short reads become available promptly; no full-chunk or batching delay.
5. Stop/cancel during body read, capacity wait, consumer wait, partial-block
   consumption, and blocked decoded-event delivery. Repeated/concurrent close,
   already-canceled context, and initialization races.
6. No double release, premature reuse, goroutine leaks, or retained block pool
   after all owners exit. Exercise with the race detector.
7. Processor paused before decoding: a compressed HTTP fixture is completely
   read/decompressed into a sufficiently large queue without invoking any
   framer or Kubernetes decoder. Insufficient capacity stops intake at the
   bound; resuming processing delivers exact objects and bookmark order.

Use real HTTP/1.1 and HTTP/2 transports to test automatic gzip and identity
responses, disabled compression, and supplied-client behavior. Assert that
enabling read-ahead does not add request headers or change existing negotiation.
Cover multiple gzip members with idle gaps, truncated/corrupt gzip, delayed
first bytes, cancellation, and compressed error/retry responses. These test
compatibility with existing Go transport behavior, not a new gzip implementation.

Run protobuf/JSON and supported CBOR correctness cases. Preserve malformed,
truncated, and oversized-frame behavior. Verify initial Adds, annotated
bookmark, subsequent Modified/Deleted events, retries, and fallback through the
reflector. Failure before the bookmark must not accept an incomplete snapshot.
Policy tests cover GVR selection, explicit zero, unknown resource, map copying,
and unchanged URLs for typed/dynamic/metadata clients.

## Implementation sequence

The ring implementation and initial comparisons now exist. The next iteration
should not restart receiver design from scratch:

1. Preserve the current source/configuration and complete workload results as
   the control. Record the budget, CPU limit, encoding, request semantics, and
   exact compiled client-go revision with each trace.
2. Keep the receiver and budget fixed. Attribute processor time to envelope
   and typed decoding, decoded-event delivery waits, allocation/GC, and
   reflector work. Record counters per completed stream/snapshot rather than
   per-event logging in timed baseline runs.
3. Prototype the narrowly scoped protobuf envelope-copy reduction described
   below, with ownership and fallback tests. Compare CPU/allocations and
   receiver capacity waiting as well as server and informer timings.
4. Prototype the etcd-inspired split processor described below as a separate
   change. Compare the existing serial path against a one-worker split path
   first, then two and four workers under the same quota. Measure copy
   reduction and concurrency separately before combining them. Adopt parallel
   decoding only if evidence and CPU capacity support it. If event-delivery
   waits dominate, investigate the initial snapshot accumulator instead.
5. Revisit resource-specific budgets against measured backlog and aggregate
   memory after processor changes. A large single-stream diagnostic can
   isolate capacity effects but is not a deployment default.
6. Publish matched per-round results and a go/no-go decision. Require lower
   server write blocking/WatchList latency, correct objects and event ordering,
   and acceptable CPU, memory, and informer-sync behavior.

Initial implementation should remain within client-go REST and relevant tests,
plus client request metadata for GVR policy. No generated files or module edits
are needed. New Go files require repository boilerplate. Follow repository
update tooling for later generated changes; do not run go mod tidy.

The processor follow-up may additionally touch apimachinery protobuf/streaming
serializer code and tests. That is separate from the original receiver-only
scope and must preserve generic serialization and input-ownership contracts.

Suggested verification, adjusted to the eventual patch:

```sh
go test ./staging/src/k8s.io/client-go/rest/...
go test ./staging/src/k8s.io/client-go/tools/cache
go test -race ./staging/src/k8s.io/client-go/rest/...
go test -race ./staging/src/k8s.io/client-go/tools/cache
go test ./staging/src/k8s.io/client-go/dynamic ./staging/src/k8s.io/client-go/metadata ./staging/src/k8s.io/client-go/gentype
git diff --check
```

Complete broader repository-required checks before submission.
For protobuf/streaming changes, also run the affected apimachinery serializer
tests and race checks, including custom-codec and buffer-reuse cases.

## Processor optimization priorities after the trace

### First candidate: eliminate intermediate protobuf payload copies

The current protobuf watch path copies embedded payload bytes while decoding
`WatchEvent.Object.Raw` through `runtime.RawExtension.Unmarshal`, then again
through `runtime.Unknown.Unmarshal` before typed-object decoding:

```text
owned frame -> WatchEvent.Object.Raw copy -> runtime.Unknown.Raw copy -> typed Pod
```

The earlier October 7 [profiling record](watchlist-remote-repro-status.md#profiling)
attributed 4.45 GiB and 4.51 GiB respectively to those allocation sites, about
37% of approximately 24 GiB allocated in that sampled process. These are
historical cumulative allocation results, not new profiles of the traced run,
not per-request allocation totals, and not a prediction of a 37% speedup.

Prototype a protobuf-specific watch decoding path that parses the envelope
payloads as views into an owned complete frame, then finishes typed decoding
before releasing or reusing that frame. Keep the raw receive queue unchanged.
This targets allocation, copying, and resulting GC competition with intake.

Requirements for this optimization:

- Restrict borrowing to codecs/types with a proved input-lifetime contract.
  Generic runtime decoders and returned RawExtensions may retain input; keep
  the existing copying fallback where ownership cannot be guaranteed.
- Returned objects must remain valid after the next frame is decoded and
  after frame storage is reused. Exercise those conditions in tests.
- Preserve protobuf envelope/GVK semantics, event validation, malformed-input
  handling, bookmarks, Status errors, and frame-size checks. Cover unknown
  fields and repeated-field semantics, not just well-formed Pod samples.
- Preserve JSON/CBOR behavior and custom-codec support.
- Keep any borrowing API/internal helper narrowly scoped. Do not globally
  change runtime.Unknown or RawExtension ownership semantics, and do not
  hand-edit generated protobuf unmarshallers.

Measure CPU and allocated bytes/objects per event, GC, receiver capacity waits,
server write blocking and WatchList latency, and informer correctness. Include
unprofiled runs. A decoder microbenchmark improvement alone is insufficient.

### Second candidate: ordered parallel typed decoding, with CPU headroom

Use the etcd watcher's ordered-processing pattern, adapted for a bounded,
allocation-sensitive client. Keep the current receiver and 4 MiB byte-ring
budget fixed for the first comparisons. This is a proposed follow-up, not a
description of the implemented ring or a measured speedup.

```text
Body.Read (transport gzip) -> bounded byte ring
                                    |
                         serial framing / envelope decode
                                    |
                         bounded owned payload tasks
                                    |
                         N persistent object-decode workers
                                    |
                         bounded ordered result slots
                                    |
                         existing ResultChan -> reflector
```

#### Stage ownership and integration

- One framing/envelope stage remains the byte ring's only consumer. Decode
  and validate the event envelope serially, then assign a monotonically
  increasing sequence number to each complete event. Do not dispatch arbitrary
  ring pages or fragments to workers.
- Dispatch owned payloads, event types, and sequence numbers. Initially use
  the existing envelope ownership behavior to isolate concurrency from copy
  reduction. Any later borrowed envelope must retain its complete owned frame
  until all users finish; workers must never reference reusable ring pages.
- Use N long-lived workers with independent safe decoder instances, or a
  decoder whose concurrency contract has been verified. Unknown/custom codecs
  retain the serial fallback unless their ownership and concurrency contracts
  are supported. Do not call the existing streaming decoder concurrently.
- Give each admitted event a reusable result slot. The ordered emitter waits
  for the next sequence, even when later slots are ready. Reuse a slot only
  after delivery/discard and after all references from its previous use end.
  Slot completion needs explicit synchronization and race tests; polling and
  a goroutine/channel allocation per event are not the intended fast path.
- Prefer a client-go-local adapter that preserves the existing `watch.Decoder`
  contract: `Decode` returns the next ordered result and `Close` cancels the
  pipeline. Keep StreamWatcher delivery and reflector semantics unchanged.
  Verify the adapter's lifetime against the existing read-ahead wrapper;
  returning from one `Decode` no longer means all processor work has stopped.

#### Admission and memory bounds

Define separate experimental processor limits before implementing the split:
N workers, K outstanding events, and F bytes of owned encoded frame/payload
capacity. Their eventual public configuration is undecided. Keep
`ExperimentalWatchListReadAheadMaxBytes` as the ring limit B; do not silently
redefine it as a total-pipeline limit.

- Start the experiment with K=N to avoid introducing a large extra backlog.
  An admission slot remains charged while queued, decoding, completed out of
  order, or awaiting delivery. Bound all those states together, not each
  channel independently. Return credit only after delivery or safe discard.
  With the proposed Decode adapter, retain credit for the returned event
  until StreamWatcher calls Decode again after its send, or Close discards it.
  Returning from Decode alone is not delivery acknowledgement. Verify this
  integration explicitly, including K=1 and a blocked ResultChan send.
- F accounts for encoded backing allocations, including spare capacity and
  retained reusable buffers, not just slice lengths. Shared views charge their
  backing allocation once; simultaneous copies charge each allocation. Release
  ownership before reuse, and do not put memory into an unaccounted global pool.
- Account separately for the framer's active assembly storage and transient
  envelope copies. Audit their existing size limits and allocation behavior;
  an input-size check alone does not prove an allocation-capacity bound.
- A valid event larger than F must not deadlock or become a new protocol error.
  Drain earlier admitted work, then process that event exclusively through the
  existing serial path and existing frame-size validation. No concurrent queued
  event may bypass this oversized-event barrier. Measure and report this
  compatibility exception rather than calling F a hard total-memory cap.
- K bounds the number of retained decoded objects, not their exact heap bytes.
  Wire size does not bound an arbitrary custom decoder's allocations. Report
  decoded heap/RSS and aggregate stream costs separately; do not claim B+F
  bounds transport buffers, decoded objects, or reflector stores.

A blocked earlier event or stalled reflector must exhaust admission credits
and eventually backpressure the ring. Moving bytes out of the ring must not
create an unbounded task or reorder queue.

#### Ordering, failure, and shutdown

All event types use the ordered path, including bookmarks and watch.Error
Status objects. Emit a bookmark only after every earlier event is delivered;
reflector processing still determines initial-snapshot completion. Preserve
existing handling of watch.Error events rather than inventing new semantics.

Represent a framing/read terminal condition at its position after all complete
preceding events. A decode failure at sequence i must follow successful events
before i and prevent delivery of later results. Preserve StreamWatcher's EOF,
unexpected-EOF, and error-reporting behavior. Do not cancel earlier work merely
because a later worker failed first.

Explicit Stop may discard pending work. Cancel admission waits, task waits,
slot waits, and body reads without needing another reflector receive. Wait for
owners to quiesce before freeing payload/result storage. A runtime.Decoder call
has no general cancellation API: do not promise to interrupt a stuck custom
decoder or recycle its input prematurely. Close must signal cancellation
promptly; deferred reclamation must account for any still-running decode.

#### Experiments and correctness gates

The earlier workload used a 1250m CPU limit and showed throttling. Confirm the
current limit rather than assuming it is unchanged. At a tight CPU quota,
workers can compete with gzip and GC without increasing total throughput;
allocation reduction remains the first candidate. Compare baseline serial,
split with one worker, then two and four workers. Do not select ten merely
because the server uses it. Keep B fixed and report K, F, oversized exceptions,
and actual retained memory; concurrency adds storage even with unchanged B.

Measure envelope/typed-decode CPU and allocations, task-admission wait,
ordered-result wait, delivery wait, ring capacity wait, GC/throttling, server
write blocking/completion, and informer sync. Stage wall times overlap; do not
sum them into a request duration. Accept the split only with reproducible
server-side benefit and acceptable memory/CPU/readiness regressions.

Required deterministic tests, in addition to the receiver tests:

1. Deliberately finish later decodes first; verify exact delivery order across
   Adds, bookmark, Modified/Deleted, and Status events.
2. Stall the first decode and independently stall the reflector. Verify K/F
   admission accounting, bounded completed results, and eventual backpressure.
3. Exercise the oversized-event serial barrier, fragmented frames, and all
   supported encodings; preserve existing malformed/oversized-frame behavior.
4. Fail framing or a later decode while earlier work is unfinished. Preserve
   the successful prefix and never emit later events past a decode failure.
5. Cancel at every admission/ownership boundary, including blocked delivery
   and an active decoder; prove no premature reuse, double release, or stuck
   pipeline-owned waits. Use controllable decoders rather than sleeps.
6. Exercise slot reuse and retained-input/custom-codec fallbacks under the
   race detector. Retaining a stopped watch must not retain reclaimable pools.

### Third candidate: reduce initial reflector/store overhead

If the decoder spends substantial time blocked delivering events, investigate
the reflector's initial temporary-store path. A specialized single-owner
snapshot accumulator could avoid some per-object locking and bookkeeping.
This is a proposed change, not an implemented or measured improvement.

Preserve key functions, transforms, duplicate/update/delete behavior, resource
versions, bookmarks, error/retry handling, and consistency checks. Do not remove
general-store synchronization globally or conflate this with post-bookmark
Replace/indexing. Optimize event handoff only with measured evidence: changing
buffer size merely relocates waiting and can add retained objects.

### Remaining diagnostic branches

First separate gzip/input costs on the receiver from framing, envelope copies,
typed decoding, GC, channel waits, and store work downstream.

- If gzip dominates intake, larger queues cannot remove that CPU cost. Report
  the limitation under the chosen transport design and test CPU headroom as a
  diagnostic; do not silently reintroduce custom decompression.
- If envelope allocations dominate, investigate the previously observed raw
  protobuf copies with explicit input ownership. Never recycle storage still
  referenced by an object. Do not hand-edit generated unmarshallers.
- If object decoding dominates and CPU is available, evaluate a bounded worker
  pool downstream of serial framing. Independent safe decoders and a sequenced
  emitter must preserve exact event order, including bookmarks and errors.
  Bound pending frame bytes, task/result counts, and decoded-object retention.
- If Replace/indexing dominates after the bookmark, optimize that stage
  separately; increasing read-ahead capacity will not directly accelerate it.

Receive blocks contain arbitrary pieces of decompressed data, not complete
events. Do not send them independently to object decoders. Any frame queue or
reordering stage belongs inside the processor and requires separate memory,
ownership, cancellation, and correctness tests.

If intake is close to the matched body-drain reference and server write blocking
and completion latency improve, the primary objective is met even if informer
synchronization is unchanged. If only the drain microbenchmark improves, the
server-side objective is not yet demonstrated. If intake misses the target
despite available capacity, inspect queue overhead and CPU contention. Prefer
measured throughput gains over a larger buffer or a more complicated
synchronization primitive.

For a submitted PR, disclose AI assistance and its scope. The human author
remains responsible for submitted changes under
[CONTRIBUTING.md](CONTRIBUTING.md).
