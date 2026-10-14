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
cluster_name=${WATCHLIST_CLUSTER_NAME:-watchlist-branch}
ssh_key=${WATCHLIST_SSH_KEY:-"$HOME/.ssh/id_rsa"}
remote_kubectl=${WATCHLIST_REMOTE_KUBECTL:-/tmp/watchlist-profile/bin/kubectl}
remote_kubeconfig=${WATCHLIST_REMOTE_KUBECONFIG:-/tmp/watchlist-profile/kubeconfig}
remote_script=${WATCHLIST_REMOTE_SCRIPT:-'~/perf-tests/clusterloader2/testing/list/watchlist-profile-remote.sh'}
remote_image=${WATCHLIST_BENCHMARK_IMAGE:-request-benchmark:watchlist-ring-improved-workers-10-c5efe113}
replicas=${WATCHLIST_REPLICAS:-10}
benchmark_mode=${WATCHLIST_MODE:-informer}
read_ahead=${WATCHLIST_ENABLE_READ_AHEAD:-true}
decoder=${WATCHLIST_READ_AHEAD_DECODER:-ring}
queue_length=${WATCHLIST_READ_AHEAD_QUEUE_LENGTH:-100}
decode_concurrency=${WATCHLIST_READ_AHEAD_DECODE_CONCURRENCY:-10}
benchmark_cpu=${WATCHLIST_CPU:-3}
log_dir=${WATCHLIST_LOG_DIR:-"$PWD/watchlist-logs-$(date -u +%Y%m%dT%H%M%SZ)"}
ssh_opts=(-i "$ssh_key" -o BatchMode=yes -o StrictHostKeyChecking=no)

mkdir -p "$log_dir"
run_log="$log_dir/clusterloader.log"
client_log="$log_dir/client-0.log"
artifact_dir="$log_dir/cl2-artifacts"

remote_run=$(printf 'WATCHLIST_CLUSTER_NAME=%q WATCHLIST_BENCHMARK_IMAGE=%q WATCHLIST_MODE=%q WATCHLIST_REPLICAS=%q WATCHLIST_CPU=%q WATCHLIST_ENABLE_READ_AHEAD=%q WATCHLIST_READ_AHEAD_DECODER=%q WATCHLIST_READ_AHEAD_QUEUE_LENGTH=%q WATCHLIST_READ_AHEAD_DECODE_CONCURRENCY=%q %s benchmark' \
  "$cluster_name" "$remote_image" "$benchmark_mode" "$replicas" "$benchmark_cpu" "$read_ahead" "$decoder" "$queue_length" "$decode_concurrency" "$remote_script")

echo "remote=$remote"
echo "cluster=$cluster_name mode=$benchmark_mode replicas=$replicas cpu=$benchmark_cpu image=$remote_image read_ahead=$read_ahead"
echo "output dir: $log_dir"
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

# CL2 writes the report directory on the remote host; retain it locally so
# latency measurements remain associated with the exact benchmark run.
report_dir=$(awk '/report: / {sub(/^.*report: /, ""); print; exit}' "$run_log")
if [[ -n "$report_dir" ]]; then
  mkdir -p "$artifact_dir"
  quoted_report_dir=$(printf '%q' "$report_dir")
  if ! ssh "${ssh_opts[@]}" "$remote" \
    "tar --no-xattrs --no-acls -C $quoted_report_dir -cf - ." \
    | tar --no-xattrs --no-acls -xf - -C "$artifact_dir"; then
    echo "warning: failed to download CL2 artifacts from $report_dir" >&2
  fi
else
  echo "warning: CL2 report directory was not found in $run_log" >&2
fi

echo "clusterloader log: $run_log"
echo "client log: $client_log"
echo "output dir: $log_dir"
if [[ -d "$artifact_dir" ]]; then
  echo "CL2 artifacts: $artifact_dir"
fi
exit "$run_status"
