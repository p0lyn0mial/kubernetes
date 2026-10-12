# WatchList envelope read-ahead: single-worker plan

Status: proposed implementation, 2026-10-12. No performance improvement has
been demonstrated for this design. This document narrows the first processor
experiment to one object-decoding worker; a parallel worker pool is deferred.

## Objective

Allow WatchEvent envelope reading to progress independently of typed Kubernetes
object decoding. The primary outcome is reduced server write blocking and
WatchList completion latency. Measure informer synchronization separately:
earlier receipt is not earlier application readiness.

Keep Go's existing HTTP transport and automatic gzip handling. Read-ahead
operates on decompressed bytes; it does not independently drain compressed
wire bytes or eliminate decompression CPU.

Related investigation:

- [Receiver and performance findings](watchlist-client-read-ahead-plan.md).
- [Bounded byte receiver design](watchlist-byte-receiver-design.md).

This is the first, simpler experiment before the multi-worker design in those
documents. It deliberately does not implement its sequence numbers, ordered
result slots, or parallel object decoding.

## Current code and baseline

At the time of inspection:

- `client-go/rest/request.go:Request.newStreamWatcher` connects `resp.Body`
  directly to the framer and streaming decoder. The earlier experimental byte
  read-ahead hook is not present in this checkout.
- `client-go/rest/watch/decoder.go:Decoder.Decode` serially decodes and validates
  a `metav1.WatchEvent`, then calls `runtime.Decode` on `got.Object.Raw`.
- `apimachinery/pkg/watch/streamwatcher.go:StreamWatcher.receive` calls that
  decoder, then sends a fully decoded `watch.Event` to its result channel.
- That result channel is currently buffered with `DefaultChanSize`, which is
  100. It buffers decoded objects, not raw envelopes.

Record the revision and configuration used for each experiment. Older design
documents describe an experimental byte receiver and an unbuffered result
channel; neither should be assumed to describe this checkout.

Keep the result-channel capacity unchanged in this experiment. If testing on
the byte-receiver branch, keep its implementation and byte budget unchanged
between controls and candidates. Do not restore or redesign that receiver as
part of this change.

## Smallest useful architecture

```text
resp.Body (transport gzip)
  -> [existing byte receiver and bounded byte queue, if enabled]
  -> envelope-reader goroutine: framing + WatchEvent decoding/validation
  -> bounded channel of owned raw events
  -> existing StreamWatcher goroutine: typed object decoding in Decode()
  -> existing ResultChan
  -> reflector
```

Add exactly one persistent goroutine per enabled stream: the envelope reader.
The existing StreamWatcher goroutine is the sole object-decoding worker. There
is no additional decoded-result channel, object-worker goroutine, goroutine per
event, result reordering, or polling loop.

With one FIFO producer and one FIFO consumer, object decoding and delivery
remain ordered naturally. The producer can read subsequent envelopes while
the consumer decodes an earlier object or waits for downstream delivery.

This creates overlap, not faster individual object decoding. When typed decode
remains the sustained bottleneck, the queue eventually fills and backpressure
returns. A tight CPU quota can make the extra concurrency neutral or harmful.

## API and integration boundary

Keep `watch.Decoder`, `watch.Interface`, StreamWatcher, and reflector contracts
unchanged. `Decode()` still returns a fully decoded object; no raw placeholders
escape through `ResultChan()`.

Introduce a separate opt-in decoder in `client-go/rest/watch`, retaining the
existing `NewDecoder` and serial implementation as the default and control.
An illustrative constructor is:

```go
NewReadAheadDecoder(streamingDecoder, objectDecoder, options)
```

The concrete options and constructor signature are implementation details.
Require positive event and payload limits when enabled; reject invalid options
before starting a goroutine. Zero/disabled configuration selects the existing
serial decoder at the call site.

Select the decoder in `Request.newStreamWatcher` after serializer negotiation.
Initially enable only for explicit WatchList requests with `sendInitialEvents`
set to true. Keep ordinary watches unchanged. Keep the selected decoder for the
whole stream, including live events following the initial bookmark; do not add
a midstream handover in version one.

Use a clearly experimental opt-in configuration path. A production-facing
option name is not settled by this document. The implementation must propagate
both limits through client configuration and request construction and test that
propagation. Do not reuse the earlier byte-ring budget name: these limits
govern a different allocation domain. A resource-specific client can choose
its limits without adding a global resource-to-budget registry.

## Queue entries and stage responsibilities

An illustrative entry is:

```go
type rawEvent struct {
    eventType watch.EventType
    payload   []byte
    err       error
    // Implementation also tracks any admission credit owned by this entry.
}
```

`err` represents a terminal envelope/framing/read failure after the preceding
successful entries. There are no sequence numbers because channel order is
sufficient.

### Envelope reader

1. Acquire one event admission credit before reading the next envelope.
2. Use the existing framer and streaming decoder with a fresh WatchEvent target.
3. Preserve the existing check that the returned object is that target and
   validate the event type against Added, Modified, Deleted, Error, and Bookmark.
4. Establish ownership of the embedded raw payload and calculate its charge.
5. Acquire payload-byte credit, then enqueue the raw event using a cancellable
   channel send. Transfer the credits with the entry.
6. Repeat. On a terminal failure, enqueue it in order and stop producing; do
   not discard preceding entries by immediately cancelling the pipeline.

Only this goroutine accesses the framer and streaming decoder. It does not
typed-decode the embedded object or inspect bookmark resource contents.

### Decode method / existing worker

1. Wait for an entry or cancellation; handle channel closure explicitly.
2. If the entry is terminal, release its credits and return the original error.
3. Otherwise call the existing `runtime.Decode(objectDecoder, payload)` once.
4. Drop the entry's raw-payload reference and release admission credits when
   the decode call finishes, on both success and failure.
5. Return the event type and decoded object, retaining existing error wrapping.

Only this consumer invokes the object decoder. On object-decode failure, stop
production and discard later queued entries safely; no later event is returned.
StreamWatcher continues to own error reporting and final event delivery.

Do not send an empty event when the channel closes. Define repeated calls after
terminal completion to return a terminal error consistently rather than block.
Concurrent calls to `Decode()` are not supported, matching its intended use.

## Ownership and codec compatibility

The envelope producer continues reading while an earlier raw payload is being
decoded. Therefore the payload must not alias reusable frame storage or mutable
storage reused by the next envelope decode.

- Retain existing envelope copies where they establish independent ownership;
  do not add a redundant copy without checking the codec's behavior.
- Audit and test the supported JSON and protobuf paths independently. Use
  fresh envelope targets, and test payload stability across subsequent reads.
- For a codec without a verified ownership guarantee, use an explicit owned
  copy or keep that codec on the serial fallback.
- Verify that envelope and object codecs do not share unsafe mutable state.
  Having only one object worker does not prove safety: envelope decoding now
  overlaps object decoding. Use independent instances where needed, or fall
  back to serial decoding for unsupported configurations.
- Do not pool or overwrite raw payload storage in version one. An arbitrary
  object decoder may retain references to its input. Dropping the queue's
  reference is safe; recycling that storage may not be.

No zero-copy work, generated protobuf edits, or serializer-wide ownership
contract changes belong in this experiment.

## Bounded admission and memory

Use two per-stream limits:

| Limit | Meaning |
| --- | --- |
| K | Maximum raw-event admissions across envelope assembly, pending byte admission, queued entries, and the active object decode |
| F | Maximum admitted raw-payload backing capacity, across queued entries and active object decode, except the oversized-event compatibility case |

Start measurements with K=2: one event can be decoded while one is being read
or queued. K=1 is a useful correctness/overhead control but prevents useful
envelope/object overlap. Compare K=8 and K=32 only after establishing the
smallest version's behavior. Queue storage may have capacity K; admission
credits, not channel length alone, enforce the aggregate K bound.

For F, use 4 MiB as an initial experimental value, not a selected production
default. It is additional to any existing 4 MiB byte-ring budget. Report both.
Charge owned backing capacity, not just payload length. If a slice retains a
larger allocation, account for that allocation or copy into accountable owned
storage. A simple credit counter protected by a mutex and a cancellable
notification mechanism is sufficient initially; no custom lock-free queue.

The payload size is known only after envelope decoding. Consequently, one
producer-held payload awaiting byte admission and the framer's assembly buffer
exist outside F. The producer must not read another envelope during that wait.
Document and measure this transient storage rather than claiming F is a strict
total-memory bound. Retain existing frame validation; the current streaming
decoder uses a 16 MiB maximum frame buffer. Verify backing allocation behavior
and any ownership-copy transients for each enabled codec.

A valid payload larger than F must not deadlock or become a new protocol error:

1. Hold it as the producer's single pending event.
2. Wait until earlier payload admissions have completed object decoding.
3. Admit it exclusively as an explicitly accounted oversized exception.
4. Wait for its decode completion before reading the next envelope.

Do not bypass FIFO order. Existing frame-size errors remain unchanged. Record
oversized exception count and peak size so frequent exceptions are visible.

Release K/F credits after object decoding, not after reflector consumption.
These limits cover the raw stage; unlike the deferred multi-worker design,
there is no reorder buffer or pending decoded-result stage inside this decoder.
Decoded objects subsequently reside in StreamWatcher, its existing bounded
result channel, the reflector, and stores. If a decoded object retains input
bytes, those bytes remain part of downstream heap, not reusable raw capacity.

Report total heap/RSS separately. F plus the byte-ring budget does not bound
decoded object allocations, transport buffers, GC-delayed reclamation, caches,
or aggregate memory across many streams.

## Ordering, errors, and cancellation

All event types follow the same path. In particular, decode watch.Error Status
objects normally; an Error event is not the same as a decoder failure. An
initial-events bookmark cannot overtake preceding objects.

Preserve the existing distinctions between EOF, unexpected EOF, transport
errors, envelope errors, and object-decode errors. Return terminal source errors
after every successfully decoded earlier event. If an earlier object's decode
fails, that failure takes precedence over later queued events/source errors.

`Close()` must be idempotent and must:

- Signal cancellation before any wait for producer shutdown.
- Close the underlying stream once, interrupting blocked reads as today.
- Wake event admission, byte admission, channel-send, and channel-receive waits.
- Allow shutdown without another reflector receive.
- Arrange release of queued payload references and credits after their owners
  stop accessing them; do not race a queue drain against an active consumer.

Keep producer termination and active decode ownership explicit. Close must not
wait on downstream delivery or on a done condition that only the calling
goroutine can satisfy. Clear retained queue references once it is safe, even
if the stopped watcher itself remains referenced.

`runtime.Decoder` has no cancellation API. A running object decode may finish
after Close; suppress subsequent delivery and retain its input until it returns.
Do not promise to interrupt a stuck custom decoder or reclaim its input early.

On a branch with byte read-ahead, audit the wrapper's cleanup assumptions:
returning from a consumer `Decode()` no longer means the envelope producer has
stopped reading. Do not release byte-ring pages until their actual reader exits.

## Implementation sequence

1. Add serial characterization tests for all event types and existing error
   behavior. Establish benchmark fixtures and baseline revision/configuration.
2. Implement the opt-in decoder and admission bookkeeping in
   `staging/src/k8s.io/client-go/rest/watch/`, with ownership and shutdown tests.
   Factor shared envelope validation only where it prevents behavior drift.
3. Add JSON/protobuf coverage and verify codec state/ownership before enabling
   each path. Keep unsupported/custom configurations on a documented fallback.
4. Add the experimental request/configuration plumbing and integration tests in
   `staging/src/k8s.io/client-go/rest/`. Preserve default serial behavior and
   ordinary-watch behavior.
5. Run targeted tests and race tests, then controlled benchmarks. Do not change
   output buffering, byte receiver, serializers, or reflector in the same test.
6. Keep the experiment opt-in until end-to-end evidence justifies adoption.
   Decide whether to retain it, tune its bounds, or stop before adding workers.

No apiserver, etcd, vendor, dependency, or generated-file edits are required.

## Correctness tests

- JSON and protobuf streams produce objects and event types identical to serial
  decoding for Added, Modified, Deleted, Error, Bookmark, and initial/live mixes.
- A blocked object decoder permits subsequent envelope progress with K>=2;
  progress stops at admission limits. K=1 completes without deadlock.
- K includes assembling, byte-waiting, queued, and actively decoded entries.
  F includes active decoding rather than only entries still in the channel.
- Many small payloads hit K; a few large payloads hit F. Each wait is cancellable.
- Oversized valid payloads run exclusively and in order; invalid oversized
  frames keep existing errors. Neither case leaks admission credits.
- Source EOF/error follows the successful prefix. An earlier object failure
  wins over later events/errors. Closed channels never produce zero-value events.
- Payloads survive subsequent envelope reads unchanged. Cover codecs that reuse
  frame storage and an object decoder retaining a payload reference.
- Stop during blocked body read, admission, channel send, empty-queue receive,
  object decode, and blocked result delivery. Repeated/concurrent Stop is safe.
- No producer leak or retained queued payloads after shutdown; active decoder
  ownership is respected. Use synchronization barriers, not timing assumptions.
- WatchList opt-in/config propagation works; disabled and ordinary-watch paths
  remain serial. Preserve initial bookmark and reflector synchronization behavior.

Suggested targeted commands from the repository root:

```sh
go test ./staging/src/k8s.io/client-go/rest/watch ./staging/src/k8s.io/client-go/rest ./staging/src/k8s.io/apimachinery/pkg/watch
go test -race ./staging/src/k8s.io/client-go/rest/watch ./staging/src/k8s.io/client-go/rest ./staging/src/k8s.io/apimachinery/pkg/watch
git diff --check
```

Add focused reflector integration coverage if request-level tests cannot prove
initial-snapshot semantics. These are planned checks, not tests already run.

## Performance experiment and decision

Compare serial decoding against split decoding with K=2 and F=4 MiB first.
Use K=1 as the no-overlap control; then vary K without changing F. Record
oversized exceptions. Do not attribute gains from increased memory to concurrency
alone: also report actual peak queued payloads and total retained heap.

Hold constant request parameters, encoding, gzip, dataset, server/client builds,
CPU quota, GOMAXPROCS, stream count, result-channel capacity, and any byte-ring
implementation/budget. Confirm the current CPU limit rather than assuming the
historical 1250m quota still applies. Use repeated runs and report variability.

Measure:

- Server write blocking and initial WatchList completion latency.
- Client time to initial bookmark and informer synchronization.
- Body bytes/read time and byte-ring space waits, if present.
- Envelope work, object decode work, raw admission waits, queue high-water marks,
  downstream delivery waits, allocation rate, GC, CPU throttling, and heap/RSS.

Stage wall times overlap; they cannot be added into a request duration. Keep
hot-path tracing optional or aggregated so instrumentation does not dominate.

The user's earlier trace showed 3.154 seconds of byte-ring space waiting in
7.898 seconds while reading about 967 MiB of decompressed data with a 4 MiB
ring. It establishes downstream backpressure, not that this particular split
will fix it. The HTTP-mode comparison also omits typed objects and informer
stores, so it remains a useful receive/envelope reference, not equivalent work.

Accept the split only if repeated end-to-end runs reduce the target server
latency beyond measurement noise with agreed memory and informer-sync guardrails.
If gains are absent, keep the serial default and use profiles to choose the next
step. Multiple workers, envelope-copy reduction, and store changes are separate
experiments, not prerequisites for completing this one.
