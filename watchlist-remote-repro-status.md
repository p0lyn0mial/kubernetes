# Remote WatchList reproduction — 2026-10-07

## Working branches

| Repository | Local path | Branch |
| --- | --- | --- |
| Kubernetes (k/k) | `/Users/lszaszki/go/src/k8s.io/kubernetes` | `watchlist-remote-repro-2026-10-08` |
| perf-tests | `/Users/lszaszki/go/src/k8s.io/perf-tests` | `watchlist-remote-repro-2026-10-08` |

## Current result

The remote kind cluster has 150,000 unscheduled Pods in
`watchlist-profile-pods-1`. CL2 runs the `testing/list/config.yaml` informer
benchmark against those Pods without recreating them. The benchmark uses
protobuf, WatchList, one informer per client, ten client Pods, and 1250m CPU /
12Gi memory per client. The host is Debian 13, 48 CPUs, 188 GiB RAM, with
Docker data under the ephemeral `/tmp/watchlist-docker`.

| Run | Pod WATCHLIST p50 | p90 | p99 | Count |
| --- | ---: | ---: | ---: | ---: |
| CI example | 13.94 s | 18.95 s | 26.60 s | 173 |
| Remote, 1 client | 8.40 s | 9.68 s | 9.97 s | 33 |
| Remote, 10 clients | 12.33 s | 14.56 s | 24.12 s | 239 |
| Remote, 10 clients, warm cache with pprof | 12.28 s | 14.46 s | 14.95 s | 240 |
| Remote, 10 clients, warm cache with cAdvisor | 12.29 s | 14.46 s | 14.95 s | 244 |

The ten-client CL2 run succeeded. Its report is
`/tmp/watchlist-profile/artifacts/cl2-benchmark-10-20261007T155738Z/` on the
remote host. The one-client report is
`/tmp/watchlist-profile/artifacts/cl2-benchmark-1-20261007T154306Z/`.
The first ten-client attempt was invalid: CL2 left an old zero-replica
Deployment in place. The remote wrapper now deletes that stopped Deployment
before each run so new image and replica settings apply. The 150,000 Pods are
never deleted by this step.
The later runs reused the API-server process and its warm cache; their lower
p99 does not establish an improvement in the client.

The CI job uses separate 32-vCPU server and worker machines; this remote run
uses one 48-vCPU kind node. The matching latency scale is useful, but the
machines are not hardware-equivalent.

## Grafana

From the Kubernetes checkout on the local machine:

```sh
./watchlist-grafana-ssh.sh
```

The script starts/reuses a remote localhost-only Grafana port-forward,
provisions the `WatchList Prometheus` data source on port 9090 and the
`WatchList remote benchmark` dashboard, then opens an SSH tunnel. It prints
the dashboard URL; with the default local port it is
`http://127.0.0.1:3000/d/watchlist-remote/watchlist-remote-benchmark`.
Pass another local port as the first argument if 3000 is occupied. Keep the
script running while using the browser. The dashboard shows Pod WatchList
p50/p90/p99 and requests per minute, plus benchmark client CPU throttling
count, percentage of periods throttled, and CPU usage against the limit.
The remote setup adds a kubelet `/metrics/cadvisor` scrape to the CL2
Prometheus because the initial API-server-only scrape omitted those series.
In 13 interior samples of the five-minute ten-client run, 57.4–58.3% of CPU
periods were throttled (mean 57.9%), with about 3,426 throttled periods per
minute across ten Pods. They used about 11.9 cores against the 12.5-core
combined limit. The percentage measures periods with throttling, not wall
time lost. The report and Prometheus query-range JSON are in
`watchlist-client-profile-2026-10-07/remote-2026-10-07/cl2-benchmark-10-20261007T161046Z/`.

The dashboard definition lives beside the remote setup script at
`perf-tests/clusterloader2/testing/list/watchlist-grafana-dashboard.json`.
The `grafana` command in `watchlist-profile-remote.sh` provisions it again
after a cluster rebuild. Prometheus and Grafana run in the cluster's
`monitoring` namespace. `/tmp` and the kind cluster are lost on remote reboot.

Raw API-server metric on the remote host:

```sh
/tmp/watchlist-profile/bin/kubectl --kubeconfig=/tmp/watchlist-profile/kubeconfig get --raw /metrics |
  awk '/^apiserver_watch_list_duration_seconds_(bucket|sum|count)\{/ && /resource="pods"/'
```

CL2 reports the histogram quantiles in
`APIResponsivenessPrometheus_*.json`; use those report values to compare
with CI rather than comparing informer sync log durations directly.

## Profiling

A second ten-client run with pprof enabled finished successfully and uses
`/tmp/watchlist-profile/artifacts/cl2-benchmark-10-20261007T160428Z/`.
One client was sampled for 45 seconds. Its CPU profile is
`client-cpu-45s.pprof`; allocation and heap profiles are in the same
directory. The sampled client allocated about 24 GiB up to 16:05:42 UTC.
The largest allocation sites were protobuf `runtime.Unknown.Unmarshal`
(4.51 GiB), `runtime.RawExtension.Unmarshal` (4.45 GiB), and typed Pod fields.
The two envelope copies together account for about 37% of allocated bytes.
The CPU profile is dominated by allocation/GC, protobuf Pod decoding, and
gzip decompression. The immediate code investigation is whether the client
can decode the two protobuf envelopes without copying their raw bytes while
preserving safety for decoders that retain input.

For a client change comparison, keep the 150,000 Pods, restart the API server
before each variant to align cache state, and run the same ten-client CL2
measurement. Save both the CL2 WatchList row and the CPU throttling series;
the CPU limit is active across all ten clients and can affect response time.

## Remote commands

```sh
ssh -o BatchMode=yes -i ~/.ssh/id_rsa lukasz.szaszkiewicz@34.118.97.146
WATCHLIST_REPLICAS=10 bash ~/perf-tests/clusterloader2/testing/list/watchlist-profile-remote.sh benchmark
bash ~/perf-tests/clusterloader2/testing/list/watchlist-profile-remote.sh grafana
```

The `benchmark` command reuses the dataset and monitoring stack. To test a
different request-benchmark image, set `WATCHLIST_BENCHMARK_IMAGE` and
`WATCHLIST_BENCHMARK_PULL_POLICY`; load the image into the kind cluster first.
CL2 accepts `CL2_BENCHMARK_PROFILE_ADDRESS=127.0.0.1:6060` for pprof.
Fetch reports and profiles from `/tmp/watchlist-profile/artifacts/` before
the host reboots.
