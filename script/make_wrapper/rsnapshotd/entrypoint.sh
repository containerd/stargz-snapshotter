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

REGISTRY_HOST=registry_integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

function check {
    if [ ${?} = 0 ] ; then
        echo "Completed: ${1}"
    else
        echo "Failed: ${1}"
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

# Log into the registry
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
check "Importing cert"

update-ca-certificates
check "Installing cert"

retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"
check "Login to the registry"

# Install remote snapshotter and filesystem plugins
mkdir -p /tmp/out
GO111MODULE=off PREFIX=/tmp/out/ make clean && \
    GO111MODULE=off PREFIX=/tmp/out/ make -j2 && \
    GO111MODULE=off PREFIX=/tmp/out/ make install
check "Installing remote snapshotter"

# Run remote snapshotter
mkdir -p /etc/rsnapshotd && \
    cp ./script/make_wrapper/rsnapshotd/config.stargz.toml /etc/rsnapshotd/config.stargz.toml
rsnapshotd --log-level=debug --config=/etc/rsnapshotd/config.stargz.toml
