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

CONTAINERD_VERSION="1.4.3"
RUNC_VERSION="v1.0.0-rc92"
SRC="${1}"
DST="${2}"
SS_REPO="/go/src/github.com/containerd/stargz-snapshotter"

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

apt-get update -y && apt-get --no-install-recommends install -y wget
update-ca-certificates
wget https://github.com/opencontainers/runc/releases/download/"${RUNC_VERSION}"/runc.amd64 -O /bin/runc
chmod 755 /bin/runc
wget https://github.com/containerd/containerd/releases/download/v"${CONTAINERD_VERSION}"/containerd-"${CONTAINERD_VERSION}"-linux-amd64.tar.gz
tar zxf containerd-1.4.3-linux-amd64.tar.gz -C /
containerd &retry ctr version

cd "${SS_REPO}"
PREFIX=/out/ make ctr-remote
/out/ctr-remote images pull "${SRC}"
/out/ctr-remote images optimize --oci "${SRC}" "${DST}"
/out/ctr-remote images push -u "${REGISTRY_CREDS}" "${DST}"
