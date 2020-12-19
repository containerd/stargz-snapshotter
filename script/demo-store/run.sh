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
REG_STORAGE_CONFIG_FILE="/etc/stargz-store/config.toml"
REG_STORAGE_ROOT=/var/lib/stargz-store/
REG_STORAGE_MOUNTPOINT="${REG_STORAGE_ROOT}store/"

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
        ps aux | grep "${1}" \
            | grep -v grep \
            | grep -v "hello.py" \
            | grep -v $(basename ${0}) \
            | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {} || true
    fi
}

function cleanup {
    podman system reset -f || true
    umount "${REG_STORAGE_MOUNTPOINT}" || true
    rm -rf "${REG_STORAGE_ROOT}"*
}

echo "cleaning up the environment..."
cleanup
kill_all "stargz-store"

echo "copying config from repo..."
cp -R "${REPO}/script/demo-store/etc/"* "/etc/"

echo "preparing commands..."
( cd "${REPO}" && PREFIX=/tmp/out/ make clean && \
      PREFIX=/tmp/out/ make -j2 && \
      PREFIX=/tmp/out/ make install )

echo "running stargz store..."
mkdir -p "${REG_STORAGE_MOUNTPOINT}"
stargz-store --log-level=debug \
             --config="${REG_STORAGE_CONFIG_FILE}" \
             "${REG_STORAGE_MOUNTPOINT}" &
retry ls "${REG_STORAGE_ROOT}store/pool"
