# Remote WatchList benchmark build

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

Build against the synced staged client-go:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 '
  set -eu
  root=/tmp/watchlist-build-current
  image=request-benchmark:watchlist-read-ahead-queue-1k-current
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

For the informer WatchList test, pass:

```text
--enable-watch-list-read-ahead=true
--watch-list-read-ahead-queue-length=1000
```

The informer logs all parsed flags at startup. The image is local to the
remote Docker daemon; the existing `watchlist-profile` kind cluster can use it
without creating a new cluster. Load it into the existing kind node before
starting the benchmark:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 \
  'sudo env PATH=/tmp/watchlist-profile/bin:$PATH kind load docker-image \
   request-benchmark:watchlist-read-ahead-queue-1k-current --name watchlist-profile'
```

Run the existing cluster with ten informer clients:

```bash
ssh lukasz.szaszkiewicz@34.118.97.146 \
  'WATCHLIST_BENCHMARK_IMAGE=request-benchmark:watchlist-read-ahead-queue-1k-current \
   WATCHLIST_MODE=informer WATCHLIST_REPLICAS=10 \
   WATCHLIST_ENABLE_READ_AHEAD=true WATCHLIST_READ_AHEAD_QUEUE_LENGTH=1000 \
   ~/perf-tests/clusterloader2/testing/list/watchlist-profile-remote.sh benchmark'
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
was alive.
