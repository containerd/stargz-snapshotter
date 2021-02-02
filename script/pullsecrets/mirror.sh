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

update-ca-certificates

cd "${SS_REPO}"
PREFIX=/out/ make ctr-remote

containerd &
retry /out/ctr-remote version
/out/ctr-remote images pull "${SRC}"
/out/ctr-remote images optimize --oci "${SRC}" "${DST}"
/out/ctr-remote images push -u "${REGISTRY_CREDS}" "${DST}"
