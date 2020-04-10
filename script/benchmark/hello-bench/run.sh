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

LEGACY_MODE="legacy"
STARGZ_MODE="stargz"
ESTARGZ_MODE="estargz"

MEASURING_SCRIPT=./script/benchmark/hello-bench/src/hello.py
REBOOT_CONTAINERD_SCRIPT=./script/benchmark/hello-bench/reboot_containerd.sh
REPO_CONFIG_DIR=./script/benchmark/hello-bench/config/
CONTAINERD_CONFIG_DIR=/etc/containerd/
REMOTE_SNAPSHOTTER_CONFIG_DIR=/etc/containerd-stargz-grpc/

if [ $# -lt 1 ] ; then
    echo "Specify benchmark target."
    echo "Ex) ${0} <YOUR_ACCOUNT_NAME> --all"
    echo "Ex) ${0} <YOUR_ACCOUNT_NAME> alpine busybox"
    exit 1
fi
TARGET_REPOUSER="${1}"
TARGET_IMAGES=${@:2}

function output {
    echo "BENCHMARK_OUTPUT: ${1}"
}

function set_noprefetch {
    local NOPREFETCH="${1}"
    sed -i 's/noprefetch = .*/noprefetch = '"${NOPREFETCH}"'/g' "${REMOTE_SNAPSHOTTER_CONFIG_DIR}config.stargz.toml"
}

NUM_OF_SUMPLES=1
function measure {
    local NAME="${1}"
    local OPTION="${2}"
    local USER="${3}"
    output "\"${NAME}\": ["
    "${MEASURING_SCRIPT}" ${OPTION} --user=${USER} --op=run --experiments=${NUM_OF_SUMPLES} ${@:4}
    output "],"
}

echo "Installing remote snapshotter..."
mkdir -p /tmp/out
PREFIX=/tmp/out/ make clean && \
    PREFIX=/tmp/out/ make -j2 && \
    PREFIX=/tmp/out/ make install
mkdir -p "${CONTAINERD_CONFIG_DIR}" && \
    cp "${REPO_CONFIG_DIR}"config.containerd.toml "${CONTAINERD_CONFIG_DIR}"
mkdir -p "${REMOTE_SNAPSHOTTER_CONFIG_DIR}" && \
    cp "${REPO_CONFIG_DIR}"config.stargz.toml "${REMOTE_SNAPSHOTTER_CONFIG_DIR}"

echo "========="
echo "SPEC LIST"
echo "========="
uname -r
cat /etc/os-release
cat /proc/cpuinfo
cat /proc/meminfo
mount
df

echo "========="
echo "BENCHMARK"
echo "========="
output "[{"

"${REBOOT_CONTAINERD_SCRIPT}" -nosnapshotter
measure "${LEGACY_MODE}" "--mode=legacy" ${TARGET_REPOUSER} ${TARGET_IMAGES}

set_noprefetch "true" # disable prefetch
"${REBOOT_CONTAINERD_SCRIPT}"
measure "${STARGZ_MODE}" "--mode=stargz" ${TARGET_REPOUSER} ${TARGET_IMAGES}

set_noprefetch "false" # enable prefetch
"${REBOOT_CONTAINERD_SCRIPT}"
measure "${ESTARGZ_MODE}" "--mode=estargz" ${TARGET_REPOUSER} ${TARGET_IMAGES}

output "}]"
