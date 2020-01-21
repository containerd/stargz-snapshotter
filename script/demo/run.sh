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

REPO=$GOPATH/src/github.com/ktock/remote-snapshotter
SNAPSHOT_NAME=remote
CONTAINERD_CONFIG_DIR=/etc/containerd/
CONTAINERD_ROOT=/var/lib/containerd/
REMOTE_SNAPSHOTTER_CONFIG_DIR=/etc/containerd-stargz-grpc/
REMOTE_SNAPSHOTTER_ROOT=/var/lib/containerd-stargz-grpc/
REMOTE_SNAPSHOTTER_SOCKET=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock

RETRYNUM=30
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    SUCCESS=false
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

function kill_all {
    if [ "${1}" != "" ] ; then
        ps aux | grep "${1}" | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {} || true
    fi
}

function cleanup {
    rm -rf "${CONTAINERD_ROOT}"*
    if [ -f "${REMOTE_SNAPSHOTTER_SOCKET}" ] ; then
        rm "${REMOTE_SNAPSHOTTER_SOCKET}"
    fi
    if [ -d "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" ] ; then 
        find "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" \
             -maxdepth 1 -mindepth 1 -type d -exec umount "{}/fs" \;
    fi
    rm -rf "${REMOTE_SNAPSHOTTER_ROOT}"*
}

echo "copying config from repo..."
mkdir -p /etc/containerd /etc/containerd-stargz-grpc && \
    cp "${REPO}/script/demo/config.containerd.toml" "${CONTAINERD_CONFIG_DIR}" && \
    cp "${REPO}/script/demo/config.stargz.toml" "${REMOTE_SNAPSHOTTER_CONFIG_DIR}"

echo "cleaning up the environment..."
kill_all "containerd"
kill_all "containerd-stargz-grpc"
cleanup

echo "preparing commands..."
( cd "${REPO}" && PREFIX=/tmp/out/ make clean && \
      PREFIX=/tmp/out/ make -j2 && \
      PREFIX=/tmp/out/ make install )

echo "running remote snaphsotter..."
containerd-stargz-grpc --log-level=debug \
                       --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
                       --config="${REMOTE_SNAPSHOTTER_CONFIG_DIR}config.stargz.toml" &
retry ls "${REMOTE_SNAPSHOTTER_SOCKET}"

echo "running containerd..."
containerd --config="${CONTAINERD_CONFIG_DIR}config.containerd.toml" $@ &
