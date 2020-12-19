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

PODMAN_CONFIG_DIR=/etc/containers/
PODMAN_STORAGE_CONFIG_FILE="${PODMAN_CONFIG_DIR}storage.conf"
REG_STORAGE_CONFIG_FILE="/etc/stargz-store/config.toml"
REG_STORAGE_ROOT=/var/lib/stargz-store/
REG_STORAGE_DIR="${REG_STORAGE_ROOT}store/"
REG_STORAGE_POOL_LINK="${REG_STORAGE_ROOT}store/pool"
REG_STORAGE_MOUNTPOINT="${REG_STORAGE_DIR}"

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
    umount "${REG_STORAGE_MOUNTPOINT}" || true
    rm -rf "${REG_STORAGE_DIR}" || true
    if [ -d "${REG_STORAGE_ROOT}pool/" ] ; then
        for POOL in $(ls "${REG_STORAGE_ROOT}pool/") ; do
            umount "${REG_STORAGE_ROOT}pool/${POOL}" || true
            for MP in $(ls "${REG_STORAGE_ROOT}pool/${POOL}") ; do
                umount "${REG_STORAGE_ROOT}pool/${POOL}/${MP}" || true
            done
        done
    fi
    rm -rf "${REG_STORAGE_ROOT}"*
    rm "${PODMAN_STORAGE_CONFIG_FILE}" || true
    podman system reset -f
}

echo "cleaning up the environment..."
kill_all "stargz-store"
cleanup

if [ "${DISABLE_PREFETCH:-}" == "true" ] ; then
    sed -i 's/noprefetch = .*/noprefetch = true/g' "${REG_STORAGE_CONFIG_FILE}"
else
    sed -i 's/noprefetch = .*/noprefetch = false/g' "${REG_STORAGE_CONFIG_FILE}"
fi

mkdir -p "${PODMAN_CONFIG_DIR}"

if [ "${DISABLE_ESTARGZ:-}" == "true" ] ; then
    echo "DO NOT RUN additional storage"
    cat <<EOF > "${PODMAN_STORAGE_CONFIG_FILE}"
[storage]
driver = "overlay"
graphroot = "/var/lib/containers/storage"
runroot = "/run/containers/storage"
EOF
else
    echo "running remote snaphsotter..."
    if [ "${LOG_FILE:-}" == "" ] ; then
        LOG_FILE=/dev/null
    fi
    cat <<EOF > "${PODMAN_STORAGE_CONFIG_FILE}"
[storage]
driver = "overlay"
graphroot = "/var/lib/containers/storage"
runroot = "/run/containers/storage"

[storage.options]
additionallayerstores = ["${REG_STORAGE_MOUNTPOINT}:ref"]
EOF
    mkdir -p "${REG_STORAGE_MOUNTPOINT}"
    stargz-store --log-level=debug \
                 --config="${REG_STORAGE_CONFIG_FILE}" \
                 "${REG_STORAGE_MOUNTPOINT}" \
                 2>&1 | tee -a "${LOG_FILE}" & # Dump all log
    retry ls "${REG_STORAGE_POOL_LINK}" > /dev/null
fi
