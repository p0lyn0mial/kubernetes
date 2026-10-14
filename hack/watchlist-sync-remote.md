# Remote WatchList benchmark build and run

`watchlist-sync-remote.sh` copies the request-benchmark source and the staged
`client-go`, `api`, `apimachinery`, and `streaming` modules to the remote build
host. It builds remotely so the binary matches the remote Linux architecture.

Prerequisites:

- the sibling `perf-tests` checkout exists;
- SSH access to the configured remote host;
- Docker and Go 1.27 images are available on the remote host.

Sync the current sources:

```bash
./hack/watchlist-sync-remote.sh
```

The defaults are the existing remote host and `/tmp/watchlist-build-current`.
Override them with `PERF_TESTS_DIR`, `WATCHLIST_REMOTE`,
`WATCHLIST_SSH_KEY`, or `WATCHLIST_REMOTE_ROOT` when needed.

The sync script copies the benchmark source and the staged Kubernetes modules;
it does not copy the whole Kubernetes checkout. The client image must be built
on the remote host because the benchmark runs in the remote Linux kind cluster.

Build against the synced staged client-go:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 '
  set -eu
  root=/tmp/watchlist-build-current
  image=request-benchmark:watchlist-ring-improved-workers-10-c5efe113
  docker run --rm -v "$root:/src" -w /src golang:1.27 \
    go work init \
      /src/perf-tests/util-images/request-benchmark \
      /src/kubernetes/staging/src/k8s.io/client-go \
      /src/kubernetes/staging/src/k8s.io/api \
      /src/kubernetes/staging/src/k8s.io/apimachinery \
      /src/kubernetes/staging/src/k8s.io/streaming
  docker run --rm -v "$root:/src" \
    -w /src/perf-tests/util-images/request-benchmark \
    -e GOWORK=/src/go.work -e GOOS=linux -e CGO_ENABLED=0 golang:1.27 \
    go build -trimpath -o benchmark .
  printf "FROM alpine:latest\\nRUN apk add --no-cache ca-certificates && update-ca-certificates\\nCOPY util-images/request-benchmark/benchmark /benchmark\\nENTRYPOINT [\\"/benchmark\\"]\\n" \
    | docker build --pull -t "$image" -f - "$root/perf-tests"
'
```

For the current informer WatchList ring test, pass:

```text
--enable-watch-list-read-ahead=true
--watch-list-read-ahead-decoder=ring
--watch-list-read-ahead-queue-length=100
--watch-list-read-ahead-decode-concurrency=10
```

The informer logs all parsed flags at startup. The image is local to the
remote Docker daemon; the existing `watchlist-profile` kind cluster can use it
without creating a new cluster. Load it into the existing kind node before
starting the benchmark:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 \
  'sudo env PATH=/tmp/watchlist-profile/bin:$PATH kind load docker-image \
   request-benchmark:watchlist-ring-improved-workers-10-c5efe113 --name watchlist-branch'
```

Run the existing `watchlist-branch` cluster with ten informer clients:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 \
  'WATCHLIST_CLUSTER_NAME=watchlist-branch \
   WATCHLIST_BENCHMARK_IMAGE=request-benchmark:watchlist-ring-improved-workers-10-c5efe113 \
   WATCHLIST_MODE=informer WATCHLIST_REPLICAS=10 WATCHLIST_CPU=3 \
   WATCHLIST_ENABLE_READ_AHEAD=true WATCHLIST_READ_AHEAD_DECODER=ring \
   WATCHLIST_READ_AHEAD_QUEUE_LENGTH=100 WATCHLIST_READ_AHEAD_DECODE_CONCURRENCY=10 \
   ~/perf-tests/clusterloader2/testing/list/watchlist-profile-remote.sh benchmark'
```

The local runner prints the complete setup before starting and streams
client 0 to a local file. Its defaults match the command above:

```bash
./hack/watchlist-run-remote.sh
```

Override any setting with `WATCHLIST_*` environment variables. For example,
to choose a separate local log directory:

```bash
WATCHLIST_LOG_DIR="$PWD/watchlist-logs-ring" ./hack/watchlist-run-remote.sh
```

The runner writes `clusterloader.log`, `client-0.log`, and a `cl2-artifacts/`
directory to that directory. The latter is a copy of the per-run CL2 report
directory, including the API responsiveness latency JSON, Prometheus snapshots,
`junit.xml`, and generated benchmark configuration.
If the runner is interrupted, remove the benchmark deployment explicitly so
the next run starts cleanly:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 \
  '/tmp/watchlist-profile/bin/kubectl --kubeconfig=/tmp/watchlist-profile/kubeconfig \
   -n list-benchmark-0 delete deployment list-pods-deploy-0 --wait=true'
```

If the dashboard is empty, check that all ten benchmark pods are `Running`
and not `ImagePullBackOff`; a locally built image must be loaded into kind.

To retain one client’s live log locally while the test runs, use the runner
script. It waits for the first benchmark pod, streams its logs, and saves the
CL2 output beside it:

```bash
./hack/watchlist-run-remote.sh
```

Set `WATCHLIST_LOG_DIR` to choose the local output directory. The client log
is not available after teardown unless it was streamed or copied while the pod
was alive. CL2 artifacts remain on the remote host after teardown and are
downloaded automatically when the run finishes.
