#!/bin/bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

set -euo pipefail

# Set environemnt variable if you want use a customize fio image,
# whose entrypoint must be the start of a fio test, and output all its logs
# (stdio (in file `stdio`), bw_log, iops_log, and lat_log) to /output.
# 256m_4t stands for 4 threads and each read 256MiB (1024MiB in total).
IMAGE_LEGACY="${IMAGE_LEGACY:-docker.io/stargz/fio:legacy_256m_4t}"
IMAGE_STARGZ="${IMAGE_STARGZ:-docker.io/stargz/fio:stargz_256m_4t}"
IMAGE_ESTARGZ="${IMAGE_ESTARGZ:-docker.io/stargz/fio:estargz_256m_4t}"

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
FS_BENCH_TOOLS_DIR=$CONTEXT/tools
REBOOT_CONTAINERD_SCRIPT=$CONTEXT/reset.sh
REMOTE_SNAPSHOTTER_CONFIG_DIR=/etc/containerd-stargz-grpc/
FIO_CONF=$CONTEXT/config/fio.conf

specList() {
  uname -r
  cat /etc/os-release
  cat /proc/cpuinfo
  cat /proc/meminfo
  mount
  df -T
}
echo "Machine spec list:"
specList

installRemoteSnapshotter() {
  which containerd-stargz-grpc && return
  local REPO_CONFIG_DIR=$CONTEXT/config/
  local CONTAINERD_CONFIG_DIR=/etc/containerd/

  mkdir -p /tmp/out
  PREFIX=/tmp/out/ make clean
  PREFIX=/tmp/out/ make -j2
  PREFIX=/tmp/out/ make install
  mkdir -p "${CONTAINERD_CONFIG_DIR}" && \
      cp "${REPO_CONFIG_DIR}"config.containerd.toml "${CONTAINERD_CONFIG_DIR}"
  mkdir -p "${REMOTE_SNAPSHOTTER_CONFIG_DIR}" && \
      cp "${REPO_CONFIG_DIR}"config.stargz.toml "${REMOTE_SNAPSHOTTER_CONFIG_DIR}"
}
echo "Installing remote snapshotter..."
installRemoteSnapshotter

set_noprefetch() {
    local NOPREFETCH="${1}"
    sed -i 's/noprefetch = .*/noprefetch = '"${NOPREFETCH}"'/g' "${REMOTE_SNAPSHOTTER_CONFIG_DIR}config.stargz.toml"
}

testLegacy() {
  local output="/output/legacy"
  local containerID="fs-bench_legacy_$(basename $(mktemp))"

  mkdir -p "$output"
  set_noprefetch "true" # disalbe prefetch

  "${REBOOT_CONTAINERD_SCRIPT}" -nosnapshotter -nocleanup
  ctr-remote image pull "$IMAGE_LEGACY"
  ctr-remote run --rm --snapshotter=overlayfs \
    --mount type=bind,src=$output,dst=/output,options=rbind:rw \
    --mount type=bind,src=$FIO_CONF,dst=/fio.conf,options=rbind:ro \
    "$IMAGE_LEGACY" "$containerID" \
    fio /fio.conf --output /output/summary.txt
}
echo "Benchmarking legacy image..."
testLegacy

testStargz() {
  local output="/output/stargz"
  local tmpdir=$(mktemp -d)
  local tmpMetrics=$(mktemp)
  local containerID="fs-bench_stargz_$(basename $(mktemp))"

  mkdir -p "$output"
  set_noprefetch "true" # disable prefetch

  "${REBOOT_CONTAINERD_SCRIPT}" "$tmpMetrics"
  ctr-remote image rpull "$IMAGE_STARGZ"
  ctr-remote run --rm --snapshotter=stargz \
    --mount type=bind,src=$output,dst=/output,options=rbind:rw \
    --mount type=bind,src=$FIO_CONF,dst=/fio.conf,options=rbind:ro \
    "$IMAGE_STARGZ" "$containerID" \
    fio /fio.conf --output /output/summary.txt
  mv "$tmpMetrics" "$output/metrics"
}
echo "Benchmarking stargz image..."
testStargz

testEstargz() {
  local output="/output/estargz"
  local tmpdir=$(mktemp -d)
  local tmpMetrics=$(mktemp)
  local containerID="fs-bench_estargz_$(basename $(mktemp))"

  mkdir -p "$output"
  set_noprefetch "false" # enable prefetch

  "${REBOOT_CONTAINERD_SCRIPT}" "$tmpMetrics"
  ctr-remote image rpull "$IMAGE_ESTARGZ"
  ctr-remote run --rm --snapshotter=stargz \
    --mount type=bind,src=$output,dst=/output,options=rbind:rw \
    --mount type=bind,src=$FIO_CONF,dst=/fio.conf,options=rbind:ro \
    "$IMAGE_ESTARGZ" "$containerID" \
    fio /fio.conf --output /output/summary.txt
  mv "$tmpMetrics" "$output/metrics"
}
echo "Benchmarking estargz image..."
testEstargz

drawProcessMetrics() {
  for c in stargz estargz ; do
    tmpdir=$(mktemp -d)
    mkdir -p $tmpdir/snapshotter
    process -input /output/$c/metrics -output $tmpdir/snapshotter \
      -config $FS_BENCH_TOOLS_DIR/process/process.conf
    pushd $tmpdir
    gnuplot <$FS_BENCH_TOOLS_DIR/plot/snapshotter.gpm >/output/$c/snapshotter.svg
    popd
    rm -rf $tmpdir
  done
}
echo "Drawing stargz-snapshotter process metrics..."
drawProcessMetrics

drawFIOMetrics() {
  for c in legacy stargz estargz ; do
    plotOutputDir=$(mktemp -d)
    $FS_BENCH_TOOLS_DIR/plot/fio.sh /output/$c $plotOutputDir
    pushd $plotOutputDir
    gnuplot <plot.gpm >/output/$c/fio.svg
    popd
    rm -rf plotOutputDir
  done
}
echo "Drawing fio metrics..."
drawFIOMetrics