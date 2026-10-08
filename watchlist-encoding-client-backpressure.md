# WatchList encoding and client backpressure

## Summary

In the no-DeepCopy benchmark run, a 150,038-Pod WatchList request took about
11 seconds on average. The response was approximately 40 MB after gzip
compression, but producing it required serializing and compressing roughly 1 GB
of protobuf input.

The profiles indicate that the server spent more time waiting for the HTTP/2
stream to accept compressed output than it spent encoding the Pod objects.
This is consistent with client or transport backpressure, but it does not by
itself identify the client-side cause.

Source run:

<https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-informer/2106327522563067904/>

Follow-up run with exact gzip and response-writer timing:

<https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-informer/2106343770336268288/>

## Average request timing

The WatchList encoder trace reported the following averages across 233
requests:

| Component | Duration | Share of encoder time |
| --- | ---: | ---: |
| Protobuf object encoding | 2.887 s | 26.1% |
| WatchEvent envelope encoding | 0.140 s | 1.3% |
| Framed write | 7.942 s | 71.9% |
| Other encoder work | 0.084 s | 0.8% |
| Total encoder time | 11.052 s | 100% |

`framedWrite` includes gzip compression, framing, response-writer wrappers,
and time blocked while the HTTP/2 stream cannot accept more output. It is not
pure CPU time and it is not equivalent to a network transfer duration.

## Exact output timing from the follow-up run

Run `2106343770336268288` added timers immediately around gzip and the
downstream `http.ResponseWriter`. It produced 226 complete, audit-ID-correlated
trace triplets. Every traced request listed 150,038 Pod events. The trace is
emitted only for requests whose total duration exceeds 10 seconds, so these
statistics describe the slow requests rather than the complete request
population.

Across those requests, the median total duration was 11.894 seconds and the
mean was 12.307 seconds. The average encoder breakdown was:

| Component | Duration | Share of encoder time |
| --- | ---: | ---: |
| Protobuf object encoding | 2.394 s | 20.0% |
| WatchEvent envelope encoding | 0.133 s | 1.1% |
| Framed write | 9.383 s | 78.3% |
| Other encoder work | 0.077 s | 0.6% |
| Total encoder time | 11.987 s | 100% |

The new output timers reported these averages:

| Component | Duration per request | Encoder-time equivalent |
| --- | ---: | ---: |
| gzip work, excluding nested response writes | 1.537 s | 12.8% |
| `http.ResponseWriter.Write` | 7.897 s | 65.9% |
| `http.Flusher.Flush` | 0.067 s | 0.6% |

The gzip trace includes final gzip flush and close work outside the per-event
encoder write, so its values do not form an exact additive subdivision of
`framedWrite`. They are close enough to answer the original question: most of
the framed-write wall time is below gzip in `ResponseWriter.Write`, while gzip
itself remains a smaller but material cost.

Each response represented about 1,005 MB before compression and 40.7 MB after
compression. The compressed output was approximately 4.05% of the input size.

### Event latency distribution

The histograms covered 33,908,588 events. Their slow tails were:

| Latency | Total encoder events | Framed-write events |
| --- | ---: | ---: |
| At least 10 ms | 30,383 | 30,373 |
| At least 25 ms | 9,259 | 9,235 |
| At least 50 ms | 619 | 617 |
| At least 100 ms | 0 | 0 |

The total and framed-write tail counts are nearly identical. This directly
locates almost every event-level outlier in the framed write rather than in
object or WatchEvent encoding. Most events were cheap: 81.11% of total event
times fell between 10 and 25 microseconds. Only 0.09% took at least 10
milliseconds, but that tail is large enough to affect the average.

### Gzip stream resets

The first 50 traced requests used one gzip stream. The following 176 requests
used between 131 and 238 streams, with a median of 176.5. A new stream is
created whenever the handler drains its input channel, flushes the response,
and later receives more initial events.

Requests with many resets averaged 202 ms in the existing aggregate flush
timer, compared with 0.84 ms for single-stream requests. Their downstream
`http.Flusher.Flush` time averaged 85.9 ms instead of 0.74 ms, and gzip time
increased from 1.509 to 1.545 seconds. Resets therefore add visible flush
overhead, but they are much smaller than the average 7.897 seconds spent in
`ResponseWriter.Write`.

The single-stream requests were the first 50 traces chronologically; all later
traces had many streams. This phase split means the lower total duration of the
many-reset group cannot be interpreted as an effect of resetting gzip. Load
and timing changed at the same point.

## CPU profile

The 30-second CPU profile contained 158.9 CPU-seconds, or an average of 5.3
cores. The principal cumulative costs were:

| Work | CPU time |
| --- | ---: |
| Watch encoder | 120.4 s |
| Protobuf object encoder | 70.5 s |
| gzip/flate | 43.3 s |
| Garbage collection | 8.5 s |
| Watch-cache iteration and production | 6.2 s |
| TLS and socket writes | about 4 s |
| `cacheWatcher.convertToWatchEvent` | 1.2 s |

Within the watch encoder CPU, protobuf serialization used about 59%, while
gzip used about 36%. Protobuf serialization performs two complete walks of
each Pod: `Pod.Size()` used 30.3 CPU-seconds and
`Pod.MarshalToSizedBuffer()` used 31.2 CPU-seconds.

Removing DeepCopy eliminated most of the previous producer and garbage
collection cost. It did not substantially reduce median request latency,
because output production and delivery remained the critical path.

## HTTP/2 blocking estimate

The block profile contains the following synchronous call path:

```text
watchEncoder
  -> gzip/flate
  -> HTTP/2 response writer
  -> writeDataFromHandler
  -> runtime.selectgo
```

The watch-encoder path accumulated approximately 1,418 seconds blocked in
HTTP/2 writes by the profile snapshot. Dividing that cumulative delay by the
approximately 212 completed benchmark requests, with another batch active,
gives an estimated 6.4-6.7 seconds of HTTP/2 blocking per request.

The follow-up run independently recorded about 1,672 cumulative seconds in
`http2.writeDataFromHandler` waiting in `runtime.selectgo` by its block-profile
snapshot. Approximately 1,634 seconds of that path passed through the new
response-writer timing wrapper. The exact request traces measured 7.897
seconds per completed slow request in that wrapper. The profile and the direct
timer therefore point to the same boundary.

Combining this estimate with the encoder trace gives this approximate split:

| Component | Duration per request | Share of encoder time |
| --- | ---: | ---: |
| Protobuf object encoding | 2.89 s | 26% |
| Waiting for HTTP/2/client progress | 6.4-6.7 s | 58-61% |
| gzip and framing while not blocked | 1.2-1.5 s | 11-14% |
| WatchEvent envelope and other work | 0.22 s | 2% |

This split is approximate. The block profile is cumulative, aggregated across
goroutines, and may include smaller non-benchmark watches. The gzip functions
appear in the blocking stack because gzip synchronously calls the downstream
HTTP/2 writer. The blocked duration must not be attributed to gzip CPU work.

### What `http2.writeDataFromHandler` does

`writeDataFromHandler` is the bridge between a `net/http` handler goroutine and
the HTTP/2 connection's frame-writing machinery. When gzip eventually writes
compressed bytes to the response, this function:

1. wraps the bytes in an HTTP/2 DATA-frame request for the current stream;
2. submits that request to the connection's serve goroutine;
3. waits in a `select` until the frame request completes, the connection is
   closed, or the stream is closed.

The connection serve goroutine places the request in its write scheduler. DATA
can be split into smaller frames, and scheduling is constrained by both the
stream-level and connection-level outbound flow-control windows. Only the final
piece signals completion back to the handler. The handler therefore cannot
return from `ResponseWriter.Write` and produce unlimited buffered output while
the connection is behind.

The `runtime.selectgo` sample below `writeDataFromHandler` means the WatchList
handler submitted output and was waiting for the HTTP/2 layer to finish that
write request. The wait can include:

- waiting for stream or connection flow-control credit from the client;
- waiting behind frames from other streams on the same connection;
- waiting for an earlier frame or buffered connection write to finish;
- waiting for the TLS or socket write when the network path is not writable;
- stream or connection shutdown.

It does not mean that `writeDataFromHandler` itself consumed CPU, and it does
not uniquely prove that protobuf decoding in the client was slow. A client that
does not drain the response promptly is one common reason flow-control credit
is returned slowly, but connection scheduling and network writes can produce
the same server-side stack.

In this benchmark, the large accumulated delay on this stack demonstrates that
the handler was repeatedly backpressured below gzip. The follow-up trace now
measures that delay directly, but additional client-side or HTTP/2
instrumentation is still needed to distinguish flow-control delay from socket
write delay and to identify why the client was not returning capacity faster.

## Interpretation

The server cannot push compressed bytes through the HTTP/2 stream as quickly
as it produces them. Possible causes include:

- the client reading the response body slowly;
- client gzip decompression or protobuf decoding;
- informer object allocation and insertion;
- client CPU throttling, garbage collection, or scheduling delays;
- exhausted HTTP/2 flow-control windows while the server waits for window
  updates;
- network or container scheduling delays.

The server block profile proves that the server waits for HTTP/2 progress. It
does not distinguish among these causes, so it is evidence of backpressure,
not proof that a specific client function is slow.

Client consumption is a plausible limiting point in this workload. The
benchmark ran ten informer replicas on one `e2-standard-32` worker. Each
replica repeatedly received about 40.7 MB of compressed data representing
roughly 1 GB and 150,038 decoded Pods. Client-go's `StreamWatcher` uses an
unbuffered result channel and does not decode the next event until the current
event has been accepted by its consumer. Decompression, protobuf decoding,
Reflector/FIFO work, or client scheduling can therefore pause response-body
reads and delay HTTP/2 window updates directly. This is a mechanism consistent
with the server evidence, not a measured client-side attribution; the run did
not collect a client CPU or blocking profile.

## Direct HTTP client result

Run `2106671033338564608` replaced the informer client with ten direct HTTP
clients. The clients decoded the protobuf WatchEvent envelopes but retained
the embedded Pod as raw bytes. This removed typed Pod decoding and informer
processing from the response-body read loop.

| Measurement | Informer client | Direct HTTP client | Change |
|---|---:|---:|---:|
| p50 | 12.604 s | 7.000 s | -44.5% |
| p90 | 14.708 s | 7.800 s | -47.0% |
| p99 | 18.878 s | 7.980 s | -57.7% |
| Completed requests | 205 | 474 | 2.31x |

The direct HTTP run's 536 completed kube-apiserver request-log entries had a
mean of 6.314 seconds, with p50 6.299 seconds, p90 6.447 seconds, p99 7.620
seconds, and a 7.729-second maximum. These exact request-log quantiles differ
from the coarser Prometheus histogram above.

All requests completed below the trace's ten-second threshold, so this run
contains no `TRACE-WATCHLIST`, `TRACE-WATCHLIST-ENCODER`, or
`TRACE-WATCHLIST-GZIP` records. The profiles still show the change in server
behavior:

| 30-second server profile | Informer client | Direct HTTP client | Change |
|---|---:|---:|---:|
| HTTP/2 `writeDataFromHandler` blocked time | 1,672 s | 200 s | -88.0% |
| `countingWriter.Write` blocked time | 1,634 s | 169 s | -89.7% |
| CPU samples | 144 s (4.80 cores) | 431 s (14.36 cores) | 2.99x |

The block-profile values are cumulative across goroutines during comparable
profile windows; they are not per-request measurements. The direct client also
completed more requests, making the reduction in blocked time per request
larger than the aggregate comparison suggests.

This is strong evidence that typed object decoding and/or informer processing
slowed response-body reads in the earlier run. Slow reads delayed HTTP/2 window
updates and made kube-apiserver's writes block. Once the direct client drained
the stream faster, kube-apiserver spent about three times as much CPU doing
encoding and compression and completed 2.31 times as many requests. The
remaining roughly 6.3 seconds includes server protobuf encoding, gzip,
transport, and WatchEvent-envelope decoding; this run cannot separate those
stages because the traces did not cross the logging threshold.

## Direct HTTP client trace breakdown

Run `2106714747968688128` lowered the slow-request trace threshold to 7.5
seconds. The kube-apiserver log contains 509 benchmark WatchList requests. Its
request-log latency distribution was p50 6.553 seconds, p90 7.514 seconds, and
p99 7.649 seconds, with a maximum of 7.714 seconds.

Fifty-seven requests, or 11.2% of the population, crossed the trace threshold.
Every one has a complete, audit-ID-correlated `TRACE-WATCHLIST`,
`TRACE-WATCHLIST-ENCODER`, and `TRACE-WATCHLIST-GZIP` triplet. The threshold
therefore captured the complete p99 tail. Each traced response contained
150,038 events.

The average traced request was divided as follows:

| Component | Duration | Per event | Share of request time |
| --- | ---: | ---: | ---: |
| Protobuf object encoding | 4.027 s | 26.842 us | 53.23% |
| WatchEvent envelope encoding | 0.215 s | 1.436 us | 2.85% |
| Framed write | 2.855 s | 19.028 us | 37.74% |
| Other encoder work | 0.125 s | 0.830 us | 1.65% |
| Work outside the encoder loop | 0.315 s | 2.100 us | 4.16% |
| Total request | 7.566 s | 50.425 us | 100% |

The gzip instrumentation further divided the framed write:

| Component | Duration per request | Share of request time |
| --- | ---: | ---: |
| gzip work, excluding nested response writes | 2.285 s | 30.20% |
| `http.ResponseWriter.Write` | 0.526 s | 6.95% |
| framing and response-writer wrapper overhead | about 0.044 s | 0.59% |
| `http.Flusher.Flush` | 0.000089 s | negligible |

The gzip and response-writer timers also include final flush and close work,
so they are not a perfectly nested subdivision of `framedWrite`. The small
arithmetic difference does not affect the conclusion: object serialization is
now the largest wall-time component, gzip is second, and downstream response
writes are no longer the limiting stage.

Each response represented about 1,005 MB before compression and 40.5 MB after
compression, a compression ratio of approximately 24.8:1.

### Direct-client event latency distribution

The 57 encoder histograms covered 8,552,166 events:

| Event duration | Cumulative share |
| --- | ---: |
| Less than 50 us | 85.64% |
| Less than 250 us | 98.20% |
| Less than 1 ms | 99.998% |

Only 166 events took at least 1 millisecond. One event took between 10 and 25
milliseconds; its recorded maximum was 11.103 milliseconds. There is no large
population of periodic multi-millisecond stalls in the direct-client run.

The framed-write histogram was even more concentrated: 89.46% of writes took
less than 10 microseconds and 99.999% took less than 1 millisecond. This differs
sharply from the informer run, where nearly every event-level outlier was in
the framed write.

### What drives the direct-client tail

Requests above 7.6 seconds averaged 7.657 seconds, compared with 7.544 seconds
for traced requests between 7.5 and 7.6 seconds. The component increases were:

| Component | 7.5-7.6 s requests | Above 7.6 s | Increase |
| --- | ---: | ---: | ---: |
| Protobuf object encoding | 4.014 s | 4.082 s | 68 ms |
| gzip | 2.280 s | 2.308 s | 28 ms |
| `http.ResponseWriter.Write` | 0.524 s | 0.532 s | 8 ms |

Object encoding had a 0.90 correlation with total request time within the
traced population and accounts for most of the difference in the slowest
requests. Gzip stream resets ranged from one to eight, with a median of one.
Their correlation with total request time was effectively zero.

Compared with the informer trace, average `http.ResponseWriter.Write` time
fell from 7.897 seconds to 0.526 seconds. Object encoding and gzip wall time
increased because the direct clients allowed kube-apiserver to keep more work
runnable concurrently; the CPU profile averaged 14.15 cores. Despite that CPU
contention, eliminating most downstream blocking reduced the traced request
mean from 12.307 seconds to 7.566 seconds.

## Experiment to locate the backpressure

Run the same WatchList request with progressively more client-side work:

1. Read and discard the compressed response body.
2. Read and gzip-decompress the body without protobuf decoding.
3. Decompress and decode all protobuf WatchEvents and Pods.
4. Run the complete informer path, including allocations and store insertion.

Record server encoder time, `framedWrite`, compressed bytes, client wall time,
and client CPU for every variant.

- If read-and-discard is substantially faster, client processing is causing
  backpressure. The first slower variant identifies the responsible stage.
- If read-and-discard remains near 12 seconds, investigate HTTP/2 flow control,
  the network path, and server-side scheduling.

The direct HTTP run establishes that the full informer path causes substantial
backpressure. Separating typed Pod decoding from informer queue and store work
still requires client variants or client-side profiles. Lowering or sampling
the server trace threshold would also expose the direct client's gzip and
downstream write breakdown.

## Follow-up evidence

- [CI result and artifacts](https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-informer/2106343770336268288/)
- [kube-apiserver log](https://storage.googleapis.com/k8s-infra-scalability-tests-logs/pull-kubernetes-benchmark-watchlist-proto-informer/2106343770336268288/benchmark-list-informer-master/kube-apiserver.log)
- [generated benchmark configuration](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-informer/2106343770336268288/artifacts/generatedConfig_list-benchmark.yaml)
- [kube-apiserver block profile](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-informer/2106343770336268288/artifacts/136.108.200.245_kube-apiserver_BlockProfile_list-benchmark_2026-10-03T12:05:33Z.pprof)
- [kube-apiserver CPU profile](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-informer/2106343770336268288/artifacts/136.108.200.245_kube-apiserver_CPUProfile_list-benchmark_2026-10-03T12:06:03Z.pprof)

## Direct HTTP client evidence

- [CI result and artifacts](https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106671033338564608/)
- [kube-apiserver log](https://storage.googleapis.com/k8s-infra-scalability-tests-logs/pull-kubernetes-benchmark-watchlist-proto-http/2106671033338564608/benchmark-list-informer-master/kube-apiserver.log)
- [generated benchmark configuration](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106671033338564608/artifacts/generatedConfig_list-benchmark.yaml)
- [kube-apiserver block profile](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106671033338564608/artifacts/35.196.14.172_kube-apiserver_BlockProfile_list-benchmark_2026-10-04T09:46:28Z.pprof)
- [kube-apiserver CPU profile](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106671033338564608/artifacts/35.196.14.172_kube-apiserver_CPUProfile_list-benchmark_2026-10-04T09:46:58Z.pprof)

## Direct HTTP client trace evidence

- [CI result and artifacts](https://gcsweb.k8s.io/gcs/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106714747968688128/)
- [kube-apiserver log](https://storage.googleapis.com/k8s-infra-scalability-tests-logs/pull-kubernetes-benchmark-watchlist-proto-http/2106714747968688128/benchmark-list-informer-master/kube-apiserver.log)
- [generated benchmark configuration](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106714747968688128/artifacts/generatedConfig_list-benchmark.yaml)
- [kube-apiserver block profile](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106714747968688128/artifacts/34.74.33.164_kube-apiserver_BlockProfile_list-benchmark_2026-10-04T12:43:36Z.pprof)
- [kube-apiserver CPU profile](https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/142631/pull-kubernetes-benchmark-watchlist-proto-http/2106714747968688128/artifacts/34.74.33.164_kube-apiserver_CPUProfile_list-benchmark_2026-10-04T12:44:06Z.pprof)
