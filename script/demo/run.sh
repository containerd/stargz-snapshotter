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

REPO=$GOPATH/src/github.com/ktock/remote-snapshotter
SNAPSHOT_NAME=remote
CONTAINERD_CONFIG_DIR=/etc/containerd/
RS_CONFIG_DIR=/etc/rs/
CONTAINERD_ROOT=/var/lib/containerd/
REMOTE_SNAPSHOTTER_ROOT=/var/lib/rs/
REMOTE_SNAPSHOTTER_SOCKET=/var/lib/rs/rs.sock

function check {
    if [ $? -ne 0 ] ; then
        (>&2 echo "Failed: ${1}")
        exit 1
    fi
}

RETRYNUM=30
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ ${i} -eq ${RETRYNUM} ] ; then
        return 1
    else
        return 0
    fi
}

function kill_all {
    if [ "${1}" != "" ] ; then
        ps aux | grep "${1}" | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {}
    fi
}

function cleanup {
    rm -rf "${CONTAINERD_ROOT}"*
    rm "${REMOTE_SNAPSHOTTER_SOCKET}"
    ls -1d "${REMOTE_SNAPSHOTTER_ROOT}io.containerd.snapshotter.v1.${SNAPSHOT_NAME}/snapshots/"* | xargs -I{} echo "{}/fs" | xargs -I{} umount {}
    rm -rf "${REMOTE_SNAPSHOTTER_ROOT}"*
}

echo "copying config from repo..."
mkdir -p /etc/containerd /etc/rs && \
    cp "${REPO}/script/demo/config.containerd.toml" "${CONTAINERD_CONFIG_DIR}" && \
    cp "${REPO}/script/demo/config.rs.toml" "${RS_CONFIG_DIR}"

echo "cleaning up the environment..."
kill_all "containerd"
kill_all "rs --log-level=debug"
cleanup

echo "preparing plugins..."
( cd "${REPO}" && PREFIX=/tmp/out/ make clean && \
      PREFIX=/tmp/out/ make -j2 && \
      PREFIX=/tmp/out/ make install )
check "Preparing plugins"

echo "running remote snaphsotter..."
rs --log-level=debug --address="${REMOTE_SNAPSHOTTER_SOCKET}" --config="${RS_CONFIG_DIR}config.rs.toml" &
retry ls "${REMOTE_SNAPSHOTTER_SOCKET}"

echo "running containerd..."
containerd --config="${CONTAINERD_CONFIG_DIR}config.containerd.toml" $@ &
