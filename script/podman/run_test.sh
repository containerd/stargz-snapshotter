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

STARGZ_STORE_SERVICE=podman-rootless-stargz-store

RETRYNUM=100
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

echo "podman unshare mount | grep stargzstore" > /tmp/test1.sh
retry bash -euo pipefail /tmp/test1.sh

# Lazy pulling and run
podman pull ghcr.io/stargz-containers/alpine:3.10.2-esgz
podman run --rm ghcr.io/stargz-containers/alpine:3.10.2-esgz echo hello

# Run (includes lazy pulling)
podman run --rm ghcr.io/stargz-containers/python:3.9-esgz echo hello

# Print store log to be checked by the host
LOG_REMOTE_SNAPSHOT="remote-snapshot-prepared"
journalctl --user -u "${STARGZ_STORE_SERVICE}" | grep "${LOG_REMOTE_SNAPSHOT}"

# Non-lazy pulling
podman run --rm ghcr.io/stargz-containers/ubuntu:22.04-org echo hello

systemctl --user stop "${STARGZ_STORE_SERVICE}"
