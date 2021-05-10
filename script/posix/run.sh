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

# Check Dockerfile at script/posix/Dockerfile
IMAGE_PJDFSTEST="${IMAGE_PJDFSTEST:-docker.io/sequix/pjdfstest:v1-stargz}"
CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"

function retry {
    local RETRYNUM=30
    local RETRYINTERVAL=1
    local TIMEOUTSEC=180
    local SUCCESS=false
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            SUCCESS=true
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ "${SUCCESS}" == "true" ] ; then
        return 0
    else
        return 1
    fi
}

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

setup() {
    local REPO_CONFIG_DIR=$CONTEXT/config/
    local CONTAINERD_CONFIG_DIR=/etc/containerd/
    local REMOTE_SNAPSHOTTER_SOCKET=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
    local REMOTE_SNAPSHOTTER_CONFIG_DIR=/etc/containerd-stargz-grpc/

    mkdir -p /tmp/out
    PREFIX=/tmp/out/ make clean
    PREFIX=/tmp/out/ make -j2
    PREFIX=/tmp/out/ make install
    mkdir -p "${CONTAINERD_CONFIG_DIR}"
    cp "${REPO_CONFIG_DIR}"config.containerd.toml "${CONTAINERD_CONFIG_DIR}"
    mkdir -p "${REMOTE_SNAPSHOTTER_CONFIG_DIR}"
    cp "${REPO_CONFIG_DIR}"config.stargz.toml "${REMOTE_SNAPSHOTTER_CONFIG_DIR}"

    containerd-stargz-grpc --log-level=debug \
        --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
        --config="${REMOTE_SNAPSHOTTER_CONFIG_DIR}config.stargz.toml" \
        &>/var/log/containerd-stargz-grpc.log &
    retry ls "${REMOTE_SNAPSHOTTER_SOCKET}"

    containerd --log-level debug \
        --config="${CONTAINERD_CONFIG_DIR}config.containerd.toml" \
        &>/var/log/containerd.log &
    retry ctr version
}
echo "Setting up stargz-snaphsotter & containerd..."
setup

testPosix() {
    local containerID="posix-test_$(basename $(mktemp))"
    ctr-remote image rpull "$IMAGE_PJDFSTEST"
    ctr-remote run --rm --snapshotter=stargz "$IMAGE_PJDFSTEST" "$containerID" >/output || \
        echo -e "\e[91mPosix test failed!\e[0m"
}
echo "Testing posix calls..."
testPosix
