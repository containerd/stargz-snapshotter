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

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/"
CRIO_SOCK=unix:///run/crio/crio.sock
PAUSE_IMAGE_NAME_PATH=/pause_name

source "${CONTEXT}/const.sh"

IMAGE_LIST="${1}"

LOG_TMP=$(mktemp)
LIST_TMP=$(mktemp)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm "${LOG_TMP}" || true
    rm "${LIST_TMP}" || true
    exit "${ORG_EXIT_CODE}"
}

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

TEST_NODE_ID=$(docker run --rm -d --privileged \
                      -v /dev/fuse:/dev/fuse \
                      --tmpfs=/var/lib/containers:suid \
                      --tmpfs=/var/lib/stargz-store:suid \
                      "${NODE_TEST_IMAGE_NAME}")
echo "Running node on: ${TEST_NODE_ID}"
retry docker exec "${TEST_NODE_ID}" /go/bin/crictl stats

# If container started successfully, varidate the runtime through CRI
FAIL=
if ! (
        echo "===== VERSION INFORMATION =====" && \
            docker exec "${TEST_NODE_ID}" runc --version && \
            docker exec "${TEST_NODE_ID}" crio --version && \
            echo "===============================" && \
            docker exec -i "${TEST_NODE_ID}" /go/bin/critest --runtime-endpoint=${CRIO_SOCK}
    ) ; then
    FAIL=true
fi

echo "Dump all names of images used in the test: ${IMAGE_LIST}"
docker exec -i "${TEST_NODE_ID}" journalctl -xu crio > "${LOG_TMP}"
cat "${LOG_TMP}" | grep "Pulling image: " | sed -E 's/.*Pulling image: ([^"]*)".*/\1/g' > "${LIST_TMP}"
docker exec -i "${TEST_NODE_ID}" cat "${PAUSE_IMAGE_NAME_PATH}" >> "${LIST_TMP}"
cat "${LIST_TMP}" | sort | uniq > "${IMAGE_LIST}"

echo "Cleaning up..."
docker kill "${TEST_NODE_ID}"

if [ "${FAIL}" != "" ] ; then
    exit 1
fi

exit 0
