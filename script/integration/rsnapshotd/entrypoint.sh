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

REGISTRY_HOST=registry-integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

RETRYNUM=100
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

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"

echo "Installing remote snapshotter and filesystem plugins..."
mkdir -p /tmp/out
GO111MODULE=off PREFIX=/tmp/out/ make clean && \
    GO111MODULE=off PREFIX=/tmp/out/ make -j2 && \
    GO111MODULE=off PREFIX=/tmp/out/ make install

echo "Running remote snapshotter..."
mkdir -p /etc/rsnapshotd && \
    cp ./script/integration/rsnapshotd/config.stargz.toml /etc/rsnapshotd/config.stargz.toml
rsnapshotd --log-level=debug --config=/etc/rsnapshotd/config.stargz.toml
