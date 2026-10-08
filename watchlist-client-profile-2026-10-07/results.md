# Local WatchList informer profile, 2026-10-07

## Setup

- One two-node kind cluster (`watchlist-client-profile`), Kubernetes v1.37.0 on Linux/arm64. The worker hosted the benchmark.
- CL2 created 50,000 unscheduled Pods from `perf-tests/clusterloader2/testing/list/pod.yaml` once. Its [rendered configuration](generatedConfig_watchlist-profile-setup.yaml) and [successful report](junit.xml) are saved here. The namespace `watchlist-profile-pods-1` remains in the cluster for more rounds.
- One `request-benchmark` informer (`--count=1`, cluster-wide Pods, protobuf, gzip, WatchList enabled), with an 8 GiB memory limit. The 1250m CPU limit matches the first local baseline. No container restarts were observed.
- Benchmark source: `kubernetes/perf-tests` at `a9a8ad7d834ecc4b8f1d6d3de1bd20ee15a14112` plus the opt-in profiler change. Go 1.26.4; `k8s.io/client-go`, `api`, and `apimachinery` v0.37.1. Local image `request-benchmark:watchlist-profile` was built as a static Linux/arm64 binary in a scratch image, ID `sha256:d037791bd5486453de4756e592bcbfae5b62be81ab475f47f6b2d827f7971e3b`.
- The API server's `apiserver_watch_list_duration_seconds_count{resource="pods",scope="cluster"}` increased during the run, confirming actual WatchList requests.

## Observations

| Client CPU limit | Completed rounds | Median sync | p90 sync | CPU throttling interval |
| --- | ---: | ---: | ---: | --- |
| 1250m | 27 | 1.601 s | 1.800 s | 78/150 periods; 2.226 s throttled over 15.041 s |
| 2 CPUs | 25 | 1.402 s | 1.502 s | 0/122 periods over 12.209 s |

The 2-CPU median was about 12% lower. The [baseline](watchlist-profile-baseline.log) and [2-CPU](watchlist-profile-2cpu.log) logs contain the individual rounds. cAdvisor counter samples are saved for [1250m](watchlist-profile-cadvisor-before.prom) and [2 CPUs](watchlist-profile-2cpu-cadvisor-before.prom), with matching `-after.prom` files. The 2-CPU working set was about 1.9 GB at one scrape, below the 8 GiB limit.

The [30-second CPU profile](watchlist-profile-cpu.pprof) contains 34.15 CPU-seconds, or about 1.14 cores. Cumulative CPU samples include 11.54 s (34%) in `Pod.Unmarshal`, 8.93 s (26%) in background GC marking, and 6.08 s (18%) under gzip reading. These call paths can overlap, so the percentages are not additive. `cache.Replace` accounts for 0.47 s (1.4%); the RealFIFO processing path accounts for 0.93 s (2.7%).

The [30-second allocation delta](watchlist-profile-allocs.pprof) estimates 32.92 GB allocated while 17 sync rounds completed, roughly 1.9 GB per snapshot. About 79% is under protobuf `Serializer.Decode`; `Pod.Unmarshal` and its nested fields account for most of that. The largest direct allocation sites include `RawExtension.Unmarshal`, `Unknown.Unmarshal`, `PodSpec.Unmarshal`, `Container.Unmarshal`, and managed-field byte cloning.

The [sampled block profile](watchlist-profile-block.pprof) records about 2.44 s in the `StreamWatcher.receive` select across 30 seconds. The much larger aggregate block totals are largely idle goroutines and the pprof request waiting for its collection interval; they are not evidence of a channel bottleneck. The CPU and allocation evidence points first to typed Pod decoding and the GC work it induces. CPU quota adds measurable latency but does not explain the full sync time.

This is one local kind run with other kind clusters sharing the Docker VM. The reported sync duration includes request and informer work; the profiles alone do not assign an exact fraction of wall time to decoding, GC, server response, or post-bookmark cache construction. The next focused experiment is to profile the same single-client workload on a remote Linux worker, or to add phase boundaries if the local-to-remote behavior differs. No HTTP comparison or optimization was attempted.

The benchmark Deployment was scaled to zero after capture. The kind cluster and `watchlist-profile-pods-1` remain available; scale `watchlist-profile-client` to one replica to resume without rerunning CL2.
