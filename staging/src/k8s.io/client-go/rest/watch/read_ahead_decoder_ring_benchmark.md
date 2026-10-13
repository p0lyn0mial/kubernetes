# Ring read-ahead decoder measurements

The `ring` implementation leaves the existing concurrent decoder intact. Select
it with `ExperimentalWatchListReadAheadDecoder: WatchListReadAheadDecoderRing`
(the selector string is `ring`). Queue and worker settings retain their existing
configuration fields.

A single reader assigns events to reusable slots in stream order. Workers take
jobs from a shared queue, so a slow decode does not reserve a worker's subsequent
jobs. The consumer waits on the next slot and returns its result before releasing
that slot for reuse. Objects returned to consumers are never recycled.

The ring bounds **all outstanding events** (raw, decoding and completed) by
`queueLength + decodeConcurrency`. The existing implementation has separate raw,
job, ordered-response and decoded-result buffers. The same queue setting therefore
does not imply identical total buffering or startup memory.

## Reproduce

From `staging/src/k8s.io/client-go`:

```sh
go test -mod=vendor -race ./rest/watch
go test -mod=vendor -race ./rest -run 'TestWatchListRingReadAhead'
go test -mod=vendor ./rest/watch -run '^$' -bench 'BenchmarkReadAhead(Pipeline|Codecs|Construction)$' -benchmem -benchtime=300ms -count=3 -cpu=3,14
```

Each pipeline/codec operation consumes one event. Initialization and warmup are
excluded. A gate prevents the reader from decoding measured events before the
timer starts, and an exact event limit prevents work beyond the measured stream.
The pipeline benchmark uses immutable, allocation-free codecs. The codec
benchmark uses real JSON/protobuf framing and a Pod with metadata, two containers,
environment variables and status. Construction is measured separately.

The allocation regression test also checks zero allocations across 10,000
steady-state events using allocation-free codecs.

## Results

Measured on 2026-10-13, Apple M4 Pro, darwin/arm64, Go 1.27.0. Values below are
medians of three 300 ms runs. `GOMAXPROCS=3` limits Go execution parallelism; it
is not a container CPU-quota simulation.

| GOMAXPROCS | Workload | Queue / workers | Concurrent ns/event | Ring ns/event | Concurrent B/event | Ring B/event | Concurrent allocs/event | Ring allocs/event |
|---|---|---|---:|---:|---:|---:|---:|---:|
| 3 | Pipeline | 100 / 10 | 1,003 | 483 | 240 | 0 | 3 | 0 |
| 3 | Pipeline | 25,000 / 6 | 1,031 | 447 | 240 | 0 | 3 | 0 |
| 3 | JSON Pod | 100 / 10 | 6,894 | 6,661 | 5,487 | 5,249 | 70 | 67 |
| 3 | JSON Pod | 25,000 / 6 | 6,437 | 6,072 | 5,485 | 5,241 | 70 | 67 |
| 3 | Protobuf Pod | 100 / 10 | 1,506 | 1,242 | 4,592 | 4,352 | 46 | 43 |
| 3 | Protobuf Pod | 25,000 / 6 | 1,264 | 996 | 4,592 | 4,352 | 46 | 43 |
| 14 | Pipeline | 100 / 10 | 1,307 | 817 | 240 | 0 | 3 | 0 |
| 14 | Pipeline | 25,000 / 6 | 1,298 | 484 | 240 | 0 | 3 | 0 |
| 14 | JSON Pod | 100 / 10 | 8,516 | 7,998 | 5,632 | 5,371 | 70 | 67 |
| 14 | JSON Pod | 25,000 / 6 | 7,323 | 6,584 | 5,542 | 5,257 | 70 | 67 |
| 14 | Protobuf Pod | 100 / 10 | 2,842 | 3,068 | 4,592 | 4,352 | 46 | 43 |
| 14 | Protobuf Pod | 25,000 / 6 | 1,771 | 1,530 | 4,592 | 4,352 | 46 | 43 |

Startup measurements at `GOMAXPROCS=14`:

| Queue / workers | Implementation | ns/construction and close | B/construction | allocs/construction |
|---|---|---:|---:|---:|
| 100 / 10 | Concurrent | 6,810 | 12,144 | 36 |
| 100 / 10 | Ring | 11,378 | 31,897 | 133 |
| 25,000 / 6 | Concurrent | 146,044 | 1,610,113 | 33 |
| 25,000 / 6 | Ring | 972,536 | 6,415,546 | 25,025 |

Zero allocations applies to the successful steady-state pipeline, not
construction, errors, framing or consumer-owned objects. The ring omits the
existing implementation's per-event timing instrumentation; both the removed
instrumentation and the changed pipeline contribute to the speed comparison.

The 14-core protobuf result with queue 100 is slower despite fewer allocations.
These short microbenchmarks establish allocation counts and illustrate throughput
under these workloads; they exclude networking, gzip, informer caching, CPU
throttling and contention across ten replicas. Confirm server and informer latency
with the deployment benchmark before drawing an end-to-end conclusion.
