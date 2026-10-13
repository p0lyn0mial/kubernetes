# Ordered versus ring read-ahead decoder

Measured on 2026-10-13, Apple M4 Pro, darwin/arm64, Go 1.27.0. Values are
medians of five 500 ms runs per configuration, without the race detector.
`GOMAXPROCS` is 3 or 14; it does not simulate a container CPU quota.

Both implementations use the same inputs, queue lengths and decode concurrency.
The pipeline benchmark uses immutable codecs that allocate nothing. The codec
benchmark includes real JSON/protobuf framing and a newly decoded Pod with labels,
two containers, environment variables and status. Each operation consumes one event.
Construction and warmup are excluded from event measurements. A gate prevents
measured events from being decoded before timing starts, and the stream limits
work to the measured event count. Cleanup finishes before the next run.

## Event processing

The ratio is ordered time divided by ring time; values above 1 favor ring.

| GOMAXPROCS | Workload | Queue / concurrency | Ordered ns/event | Ring ns/event | Ratio | Ordered B/event | Ring B/event | Ordered allocs/event | Ring allocs/event |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| 3 | Pipeline | 100 / 10 | 1,226 | 478 | 2.56 | 344 | 0 | 5 | 0 |
| 3 | Pipeline | 25,000 / 6 | 1,331 | 418 | 3.19 | 344 | 0 | 5 | 0 |
| 3 | JSON Pod | 100 / 10 | 7,426 | 6,840 | 1.09 | 5,592 | 5,248 | 72 | 67 |
| 3 | JSON Pod | 25,000 / 6 | 6,705 | 6,109 | 1.10 | 5,586 | 5,241 | 72 | 67 |
| 3 | Protobuf Pod | 100 / 10 | 3,552 | 1,231 | 2.89 | 4,696 | 4,352 | 48 | 43 |
| 3 | Protobuf Pod | 25,000 / 6 | 3,231 | 978 | 3.30 | 4,696 | 4,352 | 48 | 43 |
| 14 | Pipeline | 100 / 10 | 1,589 | 828 | 1.92 | 344 | 0 | 5 | 0 |
| 14 | Pipeline | 25,000 / 6 | 1,588 | 564 | 2.82 | 344 | 0 | 5 | 0 |
| 14 | JSON Pod | 100 / 10 | 8,720 | 8,110 | 1.08 | 5,724 | 5,384 | 72 | 67 |
| 14 | JSON Pod | 25,000 / 6 | 7,666 | 6,682 | 1.15 | 5,652 | 5,261 | 72 | 67 |
| 14 | Protobuf Pod | 100 / 10 | 5,188 | 3,021 | 1.72 | 4,696 | 4,352 | 48 | 43 |
| 14 | Protobuf Pod | 25,000 / 6 | 4,047 | 1,680 | 2.41 | 4,696 | 4,352 | 48 | 43 |

## Construction and close

This includes creating the decoder, closing it, and waiting for cleanup. The empty
stream can also schedule EOF processing, so these are lifecycle costs.

| GOMAXPROCS | Queue / concurrency | Ordered ns/op | Ring ns/op | Ordered B/op | Ring B/op | Ordered allocs/op | Ring allocs/op |
|---|---|---:|---:|---:|---:|---:|---:|
| 3 | 100 / 10 | 2,719 | 9,461 | 8,670 | 31,897 | 18 | 133 |
| 3 | 25,000 / 6 | 92,205 | 809,337 | 1,403,326 | 6,415,544 | 18 | 25025 |
| 14 | 100 / 10 | 3,539 | 11,347 | 8,676 | 31,898 | 18 | 133 |
| 14 | 25,000 / 6 | 109,790 | 880,418 | 1,403,331 | 6,415,548 | 18 | 25025 |

## Interpretation

Ring has lower median event processing time in every measured configuration.
Ordered takes 1.92–3.19 times as long in the isolated pipeline, 1.08–1.15 times
as long for JSON Pods, and 1.72–3.30 times as long for protobuf Pods.

Ordered allocates 344 bytes across five allocations per event in the isolated
pipeline; ring allocates zero. Ordered creates an envelope, a response channel,
and a goroutine for every event. Ring reuses envelopes and channels and runs a
fixed worker pool. Neither implementation records per-event timing traces.

Ordered costs less to construct. At queue length 25,000 and GOMAXPROCS=14,
construction and close allocate about 1.4 MB for ordered and 6.4 MB for ring.
Ring allocates its slots and completion channels at startup. Ordered's response
queue scales with concurrency, while its raw event buffer scales with queue length.
Equal queue settings therefore do not imply identical buffering or startup memory.

These microbenchmarks exclude HTTP, gzip, informer storage, server work and CPU
throttling. The reported ratios compare medians, without a statistical significance
test. Use deployment measurements to assess end-to-end latency and throughput.

## Reproduce

From the repository root:

```sh
GOCACHE=/tmp/watchlist-ring-go-cache go test ./staging/src/k8s.io/client-go/rest/watch -run '^$' -bench '^BenchmarkReadAhead(Pipeline|Construction)$/(ordered|ring)$' -benchmem -benchtime=500ms -count=5 -cpu=3,14
GOCACHE=/tmp/watchlist-ring-go-cache go test ./staging/src/k8s.io/client-go/rest/watch -run '^$' -bench '^BenchmarkReadAheadCodecs$/application/(json|vnd.kubernetes.protobuf)$/(ordered|ring)$' -benchmem -benchtime=500ms -count=5 -cpu=3,14
```

To check the benchmark paths with the race detector, use
`-race -benchtime=1x -count=1` instead of the timing settings above.
