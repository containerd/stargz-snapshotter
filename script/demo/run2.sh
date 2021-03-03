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

REPO=$GOPATH/src/github.com/containerd/stargz-snapshotter
STORAGE_CONFIG_DIR=/etc/registry-storage/
STORAGE_ROOT=/var/lib/registry-storage/
STORAGE_DIR=/tmp/storage/
STORAGE_MOUNTPOINT="${STORAGE_DIR}"

RETRYNUM=30
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
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

function kill_all {
    if [ "${1}" != "" ] ; then
        ps aux | grep "${1}" | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {} || true
    fi
}

function cleanup {
    umount "${STORAGE_MOUNTPOINT}" || true
    rm -rf "${STORAGE_DIR}" || true
    if [ -d "${STORAGE_ROOT}pool/" ] ; then
        for POOL in $(ls "${STORAGE_ROOT}pool/") ; do
            umount "${STORAGE_ROOT}pool/${POOL}" || true
            for MP in $(ls "${STORAGE_ROOT}pool/${POOL}") ; do
                umount "${STORAGE_ROOT}pool/${POOL}/${MP}" || true
            done
        done
    fi
    rm -rf "${STORAGE_ROOT}"*
}

echo "copying config from repo..."
mkdir -p "${STORAGE_CONFIG_DIR}" && \
    cp "${REPO}/script/demo/config.stargz.toml" "${STORAGE_CONFIG_DIR}config.toml"

echo "cleaning up the environment..."
kill_all "registry-storage"
cleanup

echo "preparing commands..."
( cd "${REPO}" && PREFIX=/tmp/out/ make clean && \
      PREFIX=/tmp/out/ make -j4 && \
      PREFIX=/tmp/out/ make install )

echo "running storage..."
mkdir -p "${STORAGE_MOUNTPOINT}"
# touch "${STORAGE_DIR}/layers.lock"
registry-storage --log-level=debug \
                 --config="${STORAGE_CONFIG_DIR}config.toml" \
                 "${STORAGE_MOUNTPOINT}" &
