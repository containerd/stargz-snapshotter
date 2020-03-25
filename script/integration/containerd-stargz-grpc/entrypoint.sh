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

SSNAPSHOTD_PID=""  # what PID needs be killed on exit (PID of stargz snapshotter daemon).
SSNAPSHOTD_ROOT=/var/lib/containerd-stargz-grpc/
function cleanup {
    ORG_EXIT_CODE="${1}"
    if [[ "${SSNAPSHOTD_PID}" != "" ]] ; then
        kill -9 "${SSNAPSHOTD_PID}" || true
    fi
    echo "Cleaning up /var/lib/containerd-stargz-grpc..."
    if [ -d "${SSNAPSHOTD_ROOT}snapshotter/snapshots/" ] ; then 
        find "${SSNAPSHOTD_ROOT}snapshotter/snapshots/" \
             -maxdepth 1 -mindepth 1 -type d -exec umount "{}/fs" \;
    fi
    rm -rf "${SSNAPSHOTD_ROOT}"*
    echo "Exit with code: ${ORG_EXIT_CODE}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"

echo "Installing remote snapshotter..."
mkdir -p /tmp/out
GO111MODULE=off PREFIX=/tmp/out/ make clean && \
    GO111MODULE=off PREFIX=/tmp/out/ make -j2 && \
    GO111MODULE=off PREFIX=/tmp/out/ make install
mkdir -p /etc/containerd-stargz-grpc && \
    cp ./script/integration/containerd-stargz-grpc/config.toml /etc/containerd-stargz-grpc/config.toml

echo "Running remote snapshotter..."
containerd-stargz-grpc --log-level=debug --config=/etc/containerd-stargz-grpc/config.toml &
SSNAPSHOTD_PID=$! # tells cleanup code what PID needs be killed on exit (via ${SSNAPSHOTD_PID}).
wait "${SSNAPSHOTD_PID}"
