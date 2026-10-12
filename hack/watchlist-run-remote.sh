#!/usr/bin/env bash
# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

remote=${WATCHLIST_REMOTE:-"lukasz.szaszkiewicz@34.118.97.146"}
ssh_key=${WATCHLIST_SSH_KEY:-"$HOME/.ssh/id_rsa"}
remote_kubectl=${WATCHLIST_REMOTE_KUBECTL:-/tmp/watchlist-profile/bin/kubectl}
remote_kubeconfig=${WATCHLIST_REMOTE_KUBECONFIG:-/tmp/watchlist-profile/kubeconfig}
remote_script=${WATCHLIST_REMOTE_SCRIPT:-'~/perf-tests/clusterloader2/testing/list/watchlist-profile-remote.sh'}
remote_image=${WATCHLIST_BENCHMARK_IMAGE:-request-benchmark:watchlist-read-ahead-queue-1k-current}
replicas=${WATCHLIST_REPLICAS:-10}
read_ahead=${WATCHLIST_ENABLE_READ_AHEAD:-true}
queue_length=${WATCHLIST_READ_AHEAD_QUEUE_LENGTH:-1000}
benchmark_cpu=${WATCHLIST_CPU:-3}
log_dir=${WATCHLIST_LOG_DIR:-"$PWD/watchlist-logs-$(date -u +%Y%m%dT%H%M%SZ)"}
ssh_opts=(-i "$ssh_key" -o BatchMode=yes -o StrictHostKeyChecking=no)

mkdir -p "$log_dir"
run_log="$log_dir/clusterloader.log"
client_log="$log_dir/client-0.log"

remote_run=$(printf 'WATCHLIST_BENCHMARK_IMAGE=%q WATCHLIST_MODE=informer WATCHLIST_REPLICAS=%q WATCHLIST_CPU=%q WATCHLIST_ENABLE_READ_AHEAD=%q WATCHLIST_READ_AHEAD_QUEUE_LENGTH=%q %s benchmark' \
  "$remote_image" "$replicas" "$benchmark_cpu" "$read_ahead" "$queue_length" "$remote_script")

echo "remote=$remote"
echo "mode=informer replicas=$replicas cpu=$benchmark_cpu image=$remote_image read_ahead=$read_ahead queue_length=$queue_length"
echo "client log: $client_log"

ssh "${ssh_opts[@]}" "$remote" "$remote_run" >"$run_log" 2>&1 &
run_pid=$!

cleanup() {
  if [[ -n ${stream_pid:-} ]] && kill -0 "$stream_pid" 2>/dev/null; then
    kill "$stream_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

ssh "${ssh_opts[@]}" "$remote" bash -s >"$client_log" 2>&1 <<REMOTE &
set -e
kubectl='$remote_kubectl'
kubeconfig='$remote_kubeconfig'
namespace=list-benchmark-0
while true; do
  pod=\$(sudo -n "\$kubectl" --kubeconfig="\$kubeconfig" -n "\$namespace" get pods \\
    -l name=list-pods-deploy-0 -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -n \$pod ]]; then
    echo "streaming pod=\$pod" >&2
    exec sudo -n "\$kubectl" --kubeconfig="\$kubeconfig" -n "\$namespace" logs --timestamps=true -f "\$pod"
  fi
  sleep 1
done
REMOTE
stream_pid=$!

run_status=0
wait "$run_pid" || run_status=$?
wait "$stream_pid" 2>/dev/null || true
trap - EXIT INT TERM

echo "clusterloader log: $run_log"
echo "client log: $client_log"
exit "$run_status"
