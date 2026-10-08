# WatchList client profiling plan

## Objective

Identify which client-side work limits WatchList throughput, first with a small
local kind experiment and then, if useful, on remote Linux machines at CI scale.

The reported CI workload has ten client Pods, each repeatedly starting an
informer, waiting for it to sync, and restarting. The dataset contains roughly
150,000 unscheduled Pods and represents about 1 GB before compression. Reported
latency is approximately 26 seconds with the informer versus 7.5 seconds p99 with
request-benchmark's HTTP WatchList mode. Confirm the metric and aggregation used
for each number before comparing them directly.

HTTP mode decodes WatchEvent envelopes but leaves embedded Pods as raw bytes.
The informer additionally decodes and retains typed Pods, then populates its
cache. The leading hypotheses are typed decoding, allocation/GC, CPU throttling,
per-event channel handoff, and store construction.

The first local objective is to explain where time goes, not to reproduce the
26-second result exactly. This document is a proposed experiment; no cluster or
profiling implementation has been created as part of writing it.

## Measurement boundaries

In the current checkout, the server's
[`WatchServer.HandleHTTP`](staging/src/k8s.io/apiserver/pkg/endpoints/handlers/watch.go)
performs the following sequence for the initial-events-end bookmark:

```text
encode/write bookmark
  -> flush if the event channel is empty
  -> record apiserver_watch_list_duration_seconds
  -> explicitly flush the initial stream
```

The metric starts at request receipt, includes APF waiting and response
backpressure, and does not wait for the client to decode the bookmark or finish
syncing. It can exclude the final explicit flush. Client processing before the
bookmark can increase the server metric through backpressure. Cache construction
after receiving the bookmark generally cannot directly increase that same
request's recorded duration. Confirm this ordering in the server revision used
for each experiment.

The client path is:

```text
HTTP body -> gzip decompression -> WatchEvent decoding -> Pod decoding
                                                           |
                                                unbuffered channel
                                                           |
                                                reflector temporary store
                                                           |
                                              initial-events-end bookmark
                                                           |
                                                 FIFO.Replace(all Pods)
                                                           |
                                              informer cache + namespace index
                                                           |
                                                       HasSynced
```

## 1. Add opt-in client profiling

Add a profiling listener to request-benchmark, available in HTTP and informer
modes, for example `--profile-address=127.0.0.1:6060`. Access it through
`kubectl port-forward`. The benchmark currently has no profiling endpoint.

Collect the following evidence in separate runs where appropriate:

| Evidence | Question answered |
| --- | --- |
| CPU profile | How much execution time goes to Pod unmarshalling, decompression, map operations, and GC? |
| Allocation profile | Which paths allocate bytes and objects? |
| Heap profile | What remains live while the snapshot is retained? |
| Sampled block profile | Is the decoder waiting to hand events to the reflector? |
| Short execution trace | Are scheduling delays, GC assists, or channel interactions limiting progress? |
| Container CPU and throttling counters | Does the CPU quota limit progress? |

CPU profiles do not account for all time when goroutines cannot run. Block and
mutex profiling require explicit runtime sampling settings; leave them disabled
in baseline runs. Interpret channel waits in context: time waiting for input is
not automatically evidence that the waiting function is the bottleneck.

Use allocation deltas over the measured interval, and report both allocated
bytes and object counts per completed snapshot or Pod. For live-heap inspection,
use a separate diagnostic run that retains the synced cache long enough to
capture it. Do not force GC in latency baseline runs.

References:

- [Go HTTP profiling endpoints](https://pkg.go.dev/net/http/pprof)
- [Go runtime profiles and sampling](https://pkg.go.dev/runtime/pprof)
- [Go execution tracing](https://pkg.go.dev/runtime/trace)

## 2. Record client phase boundaries

Record these boundaries for every round:

```text
request start -> response headers -> initial bookmark received
                                           -> HasSynced -> shutdown complete
```

Include the response's `Audit-ID`, initial object count, and elapsed durations.
Correlate the audit ID with the server's WatchList completion log. Use elapsed
durations within each process rather than assuming host clocks are identical.

| Interval | Work included |
| --- | --- |
| Request start to bookmark | Transport, decompression, decoding, event handoff, and temporary-store insertion, plus server-side waiting/work |
| Bookmark to HasSynced | Final cache construction, indexing, and sync notification |
| Shutdown and next round | Lifecycle overlap and heap turnover |

Add the informer bookmark marker at the existing reflector boundary. Avoid
adding a forwarding goroutine/channel merely to observe events, because that
would change the path under investigation. If stages overlap, do not add their
wall times as though they were sequential CPU costs.

Instrument the client-go version actually compiled into request-benchmark.
Editing k/k staging alone will not affect an image built against a published
module. Record the exact dependency revisions in the artifacts.

The current checkout contains `TRACE-STREAMWATCHER` and `TRACE-REFLECTOR`
instrumentation. Check whether the benchmark image contains it and whether its
duration thresholds suppress output for the smaller local dataset. Its response
body read timer includes automatic gzip decompression and waiting for bytes;
it is not pure network time.

## 3. Create a small kind fixture with CL2

Use one control-plane node and one worker. Place the benchmark on the worker.
Both nodes still share the Docker VM's resources; this gives placement control,
not hardware isolation.

Create a small CL2 setup configuration that reuses the existing unscheduled Pod
template from perf-tests (`clusterloader2/testing/list/pod.yaml`):

- Disable ConfigMap creation and initially create 5,000 Pods.
- Create the dataset once and reuse it across benchmark variants.
- Start with one benchmark replica and one informer or HTTP request in flight.
- Use protobuf and gzip consistently.
- Match selectors and resource-version semantics. The existing HTTP benchmark
  URI uses `resourceVersion=0`, while the informer's initial WatchList uses an
  empty RV; align those for the comparison.
- Match the CI client's CPU limit initially, after verifying the rendered CI
  deployment. The local deployment template defaults to `1250m`.
- Give the client sufficient memory to avoid memory pressure, using the same
  memory setting across variants.
- Verify that the WatchList inconsistency detector and cache mutation detector
  are disabled.

Record image revisions, Go version, effective `GOMAXPROCS`, CPU/memory limits,
Docker VM resources, negotiated content type/compression, and dataset size.
Observe server resource use and APF waiting alongside client measurements.

Increase to 10,000–20,000 Pods only if memory headroom permits and the smaller
dataset does not expose the difference. Do not start with ten clients locally.

## 4. Compare three variants

| Variant | Purpose |
| --- | --- |
| Existing HTTP WatchList | Envelope-only transfer/decoding baseline |
| Same HTTP loop, additionally decoding each Pod and discarding it | Isolate typed decoding and transient allocation |
| Existing informer WatchList | Add retention, channel handoff, and stores |

Run variants sequentially against the same dataset and repeat in alternating
order. Keep request options, concurrency, CPU/memory settings, and build settings
consistent. Retain individual request durations and separate initial cold rounds
from subsequent rounds.

Start with roughly 20–30 completed rounds per variant. Report median and spread;
do not rely on p99 from a small sample. Keep the existing immediate-restart
behavior for the baseline. Test waiting for shutdown or adding a pause only as
separate experiments, since either changes the workload.

First collect unprofiled timings. Then collect a 30-second CPU profile,
allocation data, and a separate short execution trace. Capture sampled blocking
data separately if needed. Compare profiled and unprofiled performance to detect
measurement overhead.

## 5. Select follow-up experiments from the evidence

| Finding | Next experiment |
| --- | --- |
| Pod unmarshalling dominates CPU | Quantify the added cost in typed-decode-and-discard mode |
| GC dominates | Add decode-and-retain mode; measure allocations per Pod and live heap |
| StreamWatcher send blocking dominates | Compare typed `Watch()` drained without storing; then consider a bounded-buffer experiment |
| CPU throttling is substantial | Repeat at 2 CPUs, then 4 if the host has capacity, holding server concurrency constant |
| Bookmark-to-sync dominates | Investigate replacement/indexing separately from server WatchList duration |
| Client mostly waits for input | Examine server CPU, APF, and response-writing profiles |
| Repeated rounds degrade relative to isolated rounds | Measure shutdown overlap and GC across rounds |

With current default client-go gates, initial replacement uses one RealFIFO
`ReplacedAll` event containing all objects. It should not be analyzed as 150,000
individual FIFO pops without checking the actual client version and gates.

## 6. Move to remote machines when the local result is understood

The local experiment succeeds when a specific phase explains the slowdown and
both profiles and a controlled comparison support the explanation.

Then reuse the same harness and source revisions on Linux machines with separate
server and client resources. Record architecture and rebuild matching binaries
where necessary. Increase dataset size toward 150,000 Pods with one client
first, then increase concurrency toward ten clients.

Verify whether the local explanation survives at CI scale. Keep APF waiting,
server saturation, client CPU quotas, and memory pressure visible so changes in
those conditions are not mistaken for an informer regression.

## Artifacts and completion criteria

Preserve, per experiment:

- Configuration, image/dependency revisions, dataset count, and resource limits.
- Per-round phase timings, object counts, and audit IDs.
- Correlated server WatchList timings and APF evidence.
- CPU, allocation, heap, and relevant blocking/trace artifacts.
- Client CPU usage, throttling, and memory observations.
- A comparison table describing the changed variable and resulting behavior.

The result should identify whether the dominant cost occurs before the bookmark,
after it, or across repeated rounds, and distinguish measured attribution from
remaining hypotheses. No optimization is required before that attribution is
established.
