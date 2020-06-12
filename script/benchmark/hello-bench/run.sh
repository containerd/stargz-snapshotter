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

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/"
REPO="${CONTEXT}../../../"

# NOTE: The entire contents of containerd/stargz-snapshotter are located in
# the testing container so utils.sh is visible from this script during runtime.
# TODO: Refactor the code dependencies and pack them in the container without
#       expecting and relying on volumes.
source "${REPO}/script/util/utils.sh"

MEASURING_SCRIPT="${REPO}/script/benchmark/hello-bench/src/hello.py"
REBOOT_CONTAINERD_SCRIPT="${REPO}/script/benchmark/hello-bench/reboot_containerd.sh"
REPO_CONFIG_DIR="${REPO}/script/benchmark/hello-bench/config/"
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

TMP_LOG_FILE=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${TMP_LOG_FILE}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

function output {
    echo "BENCHMARK_OUTPUT: ${1}"
}

function set_noprefetch {
    local NOPREFETCH="${1}"
    sed -i 's/noprefetch = .*/noprefetch = '"${NOPREFETCH}"'/g' "${REMOTE_SNAPSHOTTER_CONFIG_DIR}config.toml"
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

NO_STARGZ_SNAPSHOTTER="true" "${REBOOT_CONTAINERD_SCRIPT}"
measure "${LEGACY_MODE}" "--mode=legacy" ${TARGET_REPOUSER} ${TARGET_IMAGES}

echo -n "" > "${TMP_LOG_FILE}"
set_noprefetch "true" # disable prefetch
LOG_FILE="${TMP_LOG_FILE}" "${REBOOT_CONTAINERD_SCRIPT}"
measure "${STARGZ_MODE}" "--mode=stargz" ${TARGET_REPOUSER} ${TARGET_IMAGES}
check_remote_snapshots "${TMP_LOG_FILE}"

echo -n "" > "${TMP_LOG_FILE}"
set_noprefetch "false" # enable prefetch
LOG_FILE="${TMP_LOG_FILE}" "${REBOOT_CONTAINERD_SCRIPT}"
measure "${ESTARGZ_MODE}" "--mode=estargz" ${TARGET_REPOUSER} ${TARGET_IMAGES}
check_remote_snapshots "${TMP_LOG_FILE}"

output "}]"
