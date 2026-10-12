# Fast WatchList byte receiver: concrete design

Status: implemented design candidate and investigation record, updated
2026-10-12. Local microbenchmarks and a user-supplied receiver trace are recorded
below; they do not establish a general ring advantage or a completed server
latency improvement. This specification complements the
[experiment plan](watchlist-client-read-ahead-plan.md). It preserves Go's
automatic decompression and existing framing/decoding in the implemented
baseline. The proposed ordered processor follow-up is separate and not yet
implemented.

## Choice: one producer, one consumer, a paged byte ring

Use a bounded single-producer/single-consumer byte ring. The receiver reads
directly into writable ring memory and publishes the number of bytes written.
The processor consumes published bytes through the existing io.ReadCloser
interface. Channels carry wakeups only when a stage needs to park; they do not
carry payload descriptors on every read.

```text
Receiver goroutine                         Processor / StreamWatcher goroutine
  Go transport + gzip                        existing framer and decoders
          |                                             ^
  Body.Read(writable bytes)                 Read(p): copy published bytes
          |                                             |
          +------ bounded paged byte ring ---------------+
                 publish write cursor ->
                 <- publish consumed cursor
```

This is the candidate to benchmark against the current ready/free-channel
queue. Adopt it only if the additional synchronization complexity produces a
measured benefit. Keep both on identical transport, page size, byte budget,
and processor workload during comparison.

The key difference is byte-level capacity: a short body read commits only
the bytes actually read. Its unused page suffix remains available for the next
read. A descriptor queue that gives an entire block to each short read can run
out of blocks while much of its allocated payload capacity is unused.

## Findings after implementation

The user tested the paged implementation with a 1 MiB budget, then 4 MiB, and
reported no latency improvement from that increase. An early 4 MiB trace
recorded 7.898 seconds elapsed, 4.728 seconds in body reads, and 3.154 seconds
in 9,505 capacity waits, with 1,014,430,847 bytes read. The exact trace and
derived rates are in the
[workload findings](watchlist-client-read-ahead-plan.md#early-receiver-trace-capacity-waits-are-substantial).

About 40% of observed receiver wall time was spent in capacity waits. This
establishes downstream backpressure through the ring, not merely suspected
receiver synchronization overhead. The trace does not distinguish typed
decoding, GC/CPU competition, event-delivery waits, or reflector work as the
dominant downstream cause.

Go's transport still handles gzip: roughly 40 MiB on the network can become
roughly 967 MiB of body bytes at the queue. HTTP WatchList mode uses this same
decompressed-body boundary but discards each resource's raw representation
after decoding its envelope. It does not perform the informer's typed decoding
and retention. A separate receiver goroutine therefore does not provide the
same CPU/GC environment as the HTTP-only benchmark.

The 16 ms difference between elapsed time and the reported read/wait timers
does not account for all queue costs. Consumer copies, scheduling/wakeup
effects, and shared CPU/GC costs can be reflected in other intervals. Similarly,
bytes/bodyReadTime is not a measured unconstrained throughput ceiling.

The next optimization focus is
[processor allocation and decoding](watchlist-client-read-ahead-plan.md#processor-optimization-priorities-after-the-trace),
holding the ring and budget fixed to attribute changes. Do not assume another
receiver rewrite or small buffer increase will remove the observed waits.

## Local microbenchmark results

The diagnostic checkout was identified as `013376ca916`, titled
"Implement paged WatchList client read-ahead", at measurement time. Preserve
that provenance even if later history is rewritten. These results were obtained
on darwin/arm64, Apple M4 Pro, using the existing BenchmarkWatchReadAhead.

The benchmark moves 1,114,112 bytes (1.0625 MiB) from an in-memory reader,
constructing a new queue per iteration and consuming into a 64 KiB buffer.
It includes queue setup/allocation and excludes HTTP, gzip, typed decoding,
reflector processing, and the production decoder-lifecycle wrapper. The
channel control is a simplified benchmark implementation, not a drop-in
production replacement with equivalent cancellation/error behavior.

First run, default GOMAXPROCS=14, median of three repetitions:

```sh
go test ./staging/src/k8s.io/client-go/rest -run '^$' -bench '^BenchmarkWatchReadAhead$' -benchmem -benchtime=300ms -count=3
```

| Body reads / budget | Ring time | Channel time |
| --- | ---: | ---: |
| Unrestricted / 64 KiB | 64.08 microseconds | 58.77 microseconds |
| Unrestricted / 256 KiB | 57.05 microseconds | 55.59 microseconds |
| Unrestricted / 1 MiB | 94.35 microseconds | 65.97 microseconds |
| At most 4 KiB / 256 KiB | 66.27 microseconds | 89.76 microseconds |

The direct memory-reader control was about 16.92 microseconds. At the 1 MiB
budget the ring allocated about 1,049,859 bytes and 28 allocations per iteration;
the channel control allocated about 583,291 bytes and 16 allocations. These
are whole-iteration allocations, not steady-state per-read allocation counts.

A follow-up varied GOMAXPROCS; values below are approximate averages of two
repetitions, not confidence intervals:

```sh
go test ./staging/src/k8s.io/client-go/rest -run '^$' -bench '^BenchmarkWatchReadAhead/(ring-1MiB|channel-1MiB|ring-4KiB-body|channel-4KiB-body)$' -benchmem -benchtime=300ms -count=2 -cpu=1,2
```

| GOMAXPROCS / body reads / budget | Ring time | Channel time |
| --- | ---: | ---: |
| 1 / unrestricted / 1 MiB | 123.73 microseconds | 125.14 microseconds |
| 2 / unrestricted / 1 MiB | 99.38 microseconds | 76.54 microseconds |
| 1 / at most 4 KiB / 256 KiB | 76.03 microseconds | 83.15 microseconds |
| 2 / at most 4 KiB / 256 KiB | 59.77 microseconds | 87.70 microseconds |

GOMAXPROCS is not a container CPU quota. The repeated runs show that the ring
is not consistently faster than the channel control. Short-read behavior,
allocation volume, and available concurrency materially affect the comparison.
These microsecond results neither explain a multi-second workload delay by
themselves nor validate the proposed HTTP drain overhead target.

An allocation consequence follows directly from the storage layout: the ring
advances through its fixed physical capacity, allocating each page on first
visit even if unread backlog is shallow. A sufficiently long stream therefore
eventually allocates B payload bytes. The channel control can reuse fewer
blocks when producer/consumer rates permit. Under GOMAXPROCS=1 in this benchmark,
both allocated approximately the full 1 MiB budget, illustrating that this
also depends on scheduling. Avoid claiming lazy pages track peak live backlog.

## Storage and ownership

Let B be the payload budget. Split the logical ring into pages, initially
64 KiB each, with a smaller final page so total capacity is exactly B. Allocate
a fixed page-pointer table at setup and payload pages lazily on first write.
Never resize the table or move/reallocate a page during the stream. Sweep page
sizes of 16, 32, 64, and 256 KiB separately from B.

Maintain cumulative uint64 counters:

- W: bytes successfully produced and published; written only by the receiver.
- R: bytes consumed and released; written only by the processor.
- Occupied capacity is W-R, bounded by B. The producer's outstanding body read
  additionally reserves part of the free region until that read returns.

Use atomic.Uint64 for published counters. Each side maintains ordinary local
cursors, a cached peer counter, and its current physical page/offset. Refresh
the peer counter when the cache cannot satisfy the next operation. Stale
snapshots can cause an early refresh or wait; they must never authorize access
to unavailable bytes.

Counter differences use unsigned arithmetic with B below 2^63. Physical page
positions advance independently of cumulative-counter wrap: computing a
wrapped uint64 counter modulo an arbitrary B is not sufficient. Test counter
wrap with initial counters near MaxUint64 and a non-power-of-two B.

The producer owns the free region; the consumer owns the published unread
region. They may access disjoint byte ranges within the same page concurrently.
No byte may be overwritten until the consumer has published its release.
Do not append to or mutate shared slice headers. Give each operation a bounded
slice view; no caller may access spare capacity beyond its authorized range.

The producer initializes a page pointer before publishing the first bytes in
that page. The consumer observes the atomic publication before reading that
pointer or those bytes. Pointers are otherwise immutable until coordinated
shutdown. Consumer release similarly precedes producer reuse. Use Go's atomic
ordering guarantees; no unsafe pointers or hand-written memory barriers.

Separate producer- and consumer-written counters and waiting flags in memory
to limit false sharing. Choose padding from measured target architectures;
padding is a performance detail, never part of the correctness argument.

## Receiver hot path

Conceptual pseudocode; lifecycle and wait routines are specified separately:

```text
while not canceled:
    free = B - (localW - cachedR)
    if free == 0:
        cachedR = released.Load()
        if still full: parkForSpace(); continue

    dst = current page's free suffix, capped by free
    n, err = body.Read(dst)

    if n > 0:
        advance local physical cursor by n
        localW += n
        written.Store(localW)
        if consumerWaiting.Load(): tryNotify(dataReady)

    if err != nil:
        publish terminal error; close producerDone; return
    if repeated n == 0: apply bounded no-progress handling
```

If the free suffix is shorter than the normal read size, refreshing cachedR
may allow a larger read. Do not wait for a full page when some space is free.

Read directly into the ring. There is no payload enqueue copy, per-read
descriptor allocation, free-buffer channel exchange, framing, or Kubernetes
decoding here. Each successful body read requires one write-cursor publication
and a check for a sleeping consumer. Go's gzip work remains inside body.Read.

Publish every nonempty read immediately. On a short read, advance by n and
read next into the remaining suffix. Publishing a prefix does not prevent
writing a disjoint suffix concurrently with consumer reads. Never wait to
fill a page before making its prefix visible.

Do not introduce busy spinning, Gosched retry loops, timers, per-read logging,
or a worker pool for body reads. There is one sequential body and one receiver.
After the working set is allocated, this path must allocate no queue objects.
The working set can grow to the entire configured ring as the cursor advances,
even when occupancy never approaches the budget; see the allocation finding
above. Do not equate bounded allocation with small allocation.

## Processor hot path

In the implemented baseline, keep the existing StreamWatcher goroutine as the
processor. Its frame reader
uses a private io.ReadCloser adapter over the ring. This avoids changing
serializer interfaces or allowing generic decoders to retain ring storage.

Acquire a readable span from the committed prefix of the current page. Cache
that span and its local offset. A Read(p) with cached data does only cancellation
checking at the chosen bounded operation boundary, copy, and cursor advance.
It does not take a queue mutex, receive a descriptor, or load W for every small
framer read.

Publish R when the acquired span is exhausted. Bound a span by one page and
the observed committed prefix, so short published prefixes can be consumed and
released immediately. This amortizes release publication across the framer's
small reads without withholding capacity until a whole physical page is full.
Publish all consumed bytes before parking for more data. Explicit stop can
abandon the current span after processor ownership is quiescent.

When Read has copied any available bytes, it may return them immediately.
Do not wait for more input merely to fill p. Crossing into another currently
available span is optional, bounded work; benchmark it before adding it.
If no bytes are available, refresh W, inspect terminal state, then park.

The adapter still copies into the existing framer's destination buffer. This
is one copy of the decompressed stream, not an end-to-end zero-copy design.
Eliminating it would require new framing/decoder ownership contracts and is
outside this queue change. Do not expose reusable slices to runtime.Decoder.

### Follow-up: etcd-inspired ordered object decoding

The [main plan's etcd findings](watchlist-client-read-ahead-plan.md#inspiration-from-the-apiservers-etcd-receive-path)
show that the server queues serialized KV values before object decoding and
can transform them concurrently while collecting results in order. Its live
etcd watch client also has an unbounded response backlog; that part is not a
model for this bounded receiver. Initial RangeStream uses a different,
one-response buffered receive path.

The proposed client pipeline retains this ring unchanged:

```text
body reader -> byte ring -> one framing/envelope reader
                        -> bounded owned payload tasks
                        -> persistent decode workers
                        -> bounded ordered results -> StreamWatcher / reflector
```

The framing stage is still the ring's only consumer. Workers never read ring
pages directly or advance R, so the SPSC ownership proof remains unchanged.
Complete owned event payloads, not arbitrary byte chunks, cross the worker
boundary. Keep the current copies initially to measure concurrency separately
from the proposed envelope-copy optimization.

Compare serial baseline and split processor with 1/2/4 workers at fixed B.
Use reusable result slots rather than the server's per-event goroutine and
result-channel allocations. Keep delivery ordered, including bookmarks and
errors. More workers are not a throughput guarantee under a CPU quota.

Add processor admission bounds for outstanding events K and encoded backing
capacity F; B still covers only the ring. Count queued, active, completed, and
delivery-blocked events together. Account separately for frame assembly,
decoded-object heap, and the oversized-event serial compatibility exception.
Do not advertise B+F as a bound on total watch memory. See the
[processor specification](watchlist-client-read-ahead-plan.md#second-candidate-ordered-parallel-typed-decoding-with-cpu-headroom)
for admission, ordered failure, fallback, and validation requirements.

Extend lifecycle coordination before enabling this pipeline: a completed
`Decode` call no longer implies that the framing stage and workers are idle.
Close cancels every wait and body read without requiring another downstream
receive. Reclaim pages only after the ring consumer exits, and payloads/slots
only after their owners finish; a non-cancellable custom decoder cannot safely
have its input recycled while still running.

## Parking without a lost wakeup

Use two capacity-one notification channels, dataReady and spaceReady, and
one atomic waiting flag per side. A notification is a hint; the atomic cursor
predicate is the authority. Never close notification channels.

The consumer's empty-queue slow path is:

```text
publish any pending consumed bytes
consumerWaiting.Store(true)
recheck written cursor, producer completion, and cancellation
if still empty and producer live:
    select dataReady, producerDone, or canceled
consumerWaiting.Store(false)
retry the predicate
```

The receiver uses the symmetric sequence for full capacity, with
producerWaiting, released cursor, spaceReady, and cancellation.

After publishing W, the receiver checks consumerWaiting. If true, it performs
a nonblocking send to dataReady. After publishing R, the processor similarly
signals spaceReady only when producerWaiting is true.

The ordering is essential: set the waiting flag before rechecking the cursor.
If publication happens before the recheck, the waiter sees progress. If it
happens after the recheck, the publisher sees the waiting flag and signals.
A stale token is harmless: consume it and recheck. A full notification channel
already contains a wakeup. Never rely on the notification count to count bytes.

Test the exact interleavings around flag registration, predicate recheck,
notification, and select. This is not a generic multi-producer queue; adding
another producer or consumer would invalidate these ownership assumptions.

## EOF, errors, cancellation, and reclamation

Terminal errors are stored out of band. The producer first publishes all bytes
returned with the error, then stores the error and closes producerDone. This
does not need a free payload slot or descriptor, even when the ring is full.

The consumer drains committed bytes before reporting natural EOF/error. After
observing producerDone it must reload W before deciding no data remains: a
previous empty snapshot may predate the final publication. The error field is
read only after observing producerDone, providing publication synchronization.

Explicit Close cancels waits and closes the underlying body exactly once.
It does not require draining, a future reflector receive, or another Decode.
Cancellation is checked at receiver iterations and bounded consumer operation
boundaries, so buffered bytes do not defer shutdown indefinitely.

Cleanup must observe both receiver exit and cessation of processor access.
The current decoder wrapper's lifecycle synchronization is a starting point:
close input before coordinating with an active Decode; cover the case where
Decode returned and StreamWatcher is blocked sending an event. Do not replace
this with unsynchronized cursor/page clearing for speed.

Prevent new Decode entry once shutdown owns final cleanup. After the receiver
and active processor operation exit, clear page references exactly once.
Do not free ring pages on producer EOF alone: unread bytes still belong to the
processor. Avoid a permanent third goroutine; any shutdown-only waiter must
terminate and must not wait for another downstream receive.

## Capacity determines how long the server stays unblocked

Reducing queue overhead does not remove a slower processor. For a finite
initial stream of S decompressed bytes, receiver rate V and processor rate P,
the idealized backlog at unthrottled receive completion is:

```text
B_needed ≈ max(0, S * (1 - P/V))
T_receive >= max(S/V, (S-B)/P)      for S > B, P > 0
```

These are constant-rate estimates, not latency predictions. Real gzip, CPU
quotas, server scheduling, frame sizes, and stores vary over time. Capacity
withheld by a partially consumed span also reduces effective free space.

Illustrative example only: S=1024 MiB, V=160 MiB/s, P=100 MiB/s. Receiving at
the unconstrained rate takes 6.4 seconds and leaves about 384 MiB to process.
A 16 MiB queue fills after roughly 0.27 seconds of that imbalance and gives an
idealized receive-time lower bound of 10.08 seconds. Making its atomic operations
faster cannot turn that into 6.4 seconds.

Use per-resource budgets from measured backlog, within aggregate client memory
limits. Do not silently allocate a whole snapshot to achieve an attractive
server latency number. If the budget needed is too large, increase processor
throughput rather than claiming that a faster queue solves the imbalance.

## Validation and adoption criteria

Compare this ring with the current channel queue, not just the unbuffered
watch path. Required cases include:

- Direct drain of the same automatically decompressed body; receiver only
  with a paused processor and a modest fixture fitting within B; full pipeline.
- Full/short/one-byte body reads, variable framer read sizes, sparse events,
  page/ring boundaries, budgets smaller than a page, partial last pages, and
  uint64 cursor wrap with non-power-of-two capacity.
- Independent producer/consumer pauses: no lost wakeups, no overwrite of live
  bytes, no reads of unpublished bytes, and no allocation beyond B.
- EOF/data-plus-error at full capacity, cancellation at every wait/ownership
  boundary, already-canceled contexts, repeated Close, blocked event delivery,
  and cleanup while retaining the stopped watch reference.
- Race-detector runs, randomized data-integrity stress, and deterministic
  scheduling tests for parking/terminal publication.
- HTTP/1.1 and HTTP/2 with normal gzip, the actual CPU quota, and representative
  resource sizes and concurrent watches. Preserve event/bookmark/error order.

Target queue-only steady-state allocations at zero, and receiver time within
5% of the matched decompressed-body drain for the fitting fixture. Profile
atomic/cache traffic and parked wakeups; no claim that this ring wins follows
from calling it single-producer/single-consumer.

The adoption result must show lower server write blocking and WatchList
completion latency with acceptable aggregate memory, CPU, informer-sync delay,
and steady-state event latency. If the channel queue performs equally well,
keep it: the ring's extra correctness burden must buy a measurable result.
