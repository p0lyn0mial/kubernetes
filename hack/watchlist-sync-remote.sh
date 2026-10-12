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

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
kubernetes_dir=$(cd "$script_dir/.." && pwd)
perf_tests_dir=${PERF_TESTS_DIR:-"$kubernetes_dir/../perf-tests"}
remote=${WATCHLIST_REMOTE:-"lukasz.szaszkiewicz@34.118.97.146"}
ssh_key=${WATCHLIST_SSH_KEY:-"$HOME/.ssh/id_rsa"}
remote_root=${WATCHLIST_REMOTE_ROOT:-"/tmp/watchlist-build-current"}
ssh_opts=(-i "$ssh_key" -o StrictHostKeyChecking=no)

die() { echo "error: $*" >&2; exit 1; }

[[ -d "$perf_tests_dir/util-images/request-benchmark" ]] || die "missing perf-tests at $perf_tests_dir"
[[ -d "$kubernetes_dir/staging/src/k8s.io/client-go" ]] || die "missing staged client-go"
[[ -f "$perf_tests_dir/clusterloader2/testing/list/deployment.yaml" ]] || die "missing WatchList benchmark deployment template"
rg -q -- '--enable-watch-list-read-ahead=' "$perf_tests_dir/clusterloader2/testing/list/deployment.yaml" || die "deployment template does not pass --enable-watch-list-read-ahead"
rg -q -- '--watch-list-read-ahead-queue-length=' "$perf_tests_dir/clusterloader2/testing/list/deployment.yaml" || die "deployment template does not pass --watch-list-read-ahead-queue-length"
command -v tar >/dev/null || die "tar is required"

ssh "${ssh_opts[@]}" "$remote" "mkdir -p '$remote_root/perf-tests/util-images' '$remote_root/perf-tests/clusterloader2/testing/list' '$remote_root/kubernetes/staging/src/k8s.io'"

copy_path() {
  local source=$1 destination=$2
  if [[ -d "$source" ]]; then
    tar --no-xattrs --no-acls -C "$source" -cf - . | ssh "${ssh_opts[@]}" "$remote" \
      "mkdir -p '$destination' && tar --no-xattrs --no-acls -xf - -C '$destination'"
  else
    tar --no-xattrs --no-acls -C "$(dirname "$source")" -cf - "$(basename "$source")" | ssh "${ssh_opts[@]}" "$remote" \
      "mkdir -p '$destination' && tar --no-xattrs --no-acls -xf - -C '$destination'"
  fi
}

copy_path "$perf_tests_dir/util-images/request-benchmark" \
  "$remote_root/perf-tests/util-images/request-benchmark"

for module in client-go api apimachinery streaming; do
  copy_path "$kubernetes_dir/staging/src/k8s.io/$module" \
    "$remote_root/kubernetes/staging/src/k8s.io/$module"
done

for file in deployment.yaml watchlist-profile-remote.sh; do
  copy_path "$perf_tests_dir/clusterloader2/testing/list/$file" \
    "$remote_root/perf-tests/clusterloader2/testing/list"
done

ssh "${ssh_opts[@]}" "$remote" "cd '$remote_root' && sha256sum perf-tests/util-images/request-benchmark/*.go kubernetes/staging/src/k8s.io/client-go/*.go"
echo "Synced benchmark source and staged client-go modules to $remote:$remote_root"
