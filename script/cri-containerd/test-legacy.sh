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
REPO="${CONTEXT}../../"
CONTAINERD_SOCK=unix:///run/containerd/containerd.sock
IMAGE_ENDPOINT_SOCK=unix:///run/containerd-stargz-grpc/containerd-stargz-grpc.sock
if [ "${FUSE_MANAGER:-}" == "true" ] ; then
    IMAGE_ENDPOINT_SOCK=unix:///run/containerd-stargz-grpc/cri.sock
fi

source "${CONTEXT}/const.sh"
source "${REPO}/script/util/utils.sh"

IMAGE_LIST="${1}"

PAUSE_IMAGE_NAME=$(get_version_from_arg "${REPO}/Dockerfile" "PAUSE_IMAGE_NAME_TEST")

LOG_TMP=$(mktemp)
LIST_TMP=$(mktemp)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm "${LOG_TMP}" || true
    rm "${LIST_TMP}" || true
    exit "${ORG_EXIT_CODE}"
}

TEST_NODE_ID=$(docker run --rm -d --privileged \
                      -v /dev/fuse:/dev/fuse \
                      --tmpfs=/var/lib/containerd:suid \
                      --tmpfs=/var/lib/containerd-stargz-grpc:suid \
                      "${NODE_TEST_IMAGE_NAME}")
echo "Running node on: ${TEST_NODE_ID}"
FAIL=
for i in $(seq 100) ; do
    if docker exec -i "${TEST_NODE_ID}" ctr version ; then
        break
    fi
    echo "Fail(${i}). Retrying..."
    if [ $i == 100 ] ; then
        FAIL=true
    fi
    sleep 1
done

# If container started successfully, varidate the runtime through CRI
if [ "${FAIL}" == "" ] ; then
    if ! (
            echo "===== VERSION INFORMATION =====" && \
                docker exec "${TEST_NODE_ID}" runc --version && \
                docker exec "${TEST_NODE_ID}" containerd --version && \
                echo "===============================" && \
                # FIXME: remove the skip flag once kind adds support for the user namespace
                # See also https://github.com/kubernetes-sigs/kind/issues/3436
                docker exec -i "${TEST_NODE_ID}" /go/bin/critest \
                       --runtime-endpoint=${CONTAINERD_SOCK} --image-endpoint=${IMAGE_ENDPOINT_SOCK} \
                       --ginkgo.skip 'runtime should support NamespaceMode_POD'
        ) ; then
        FAIL=true
    fi
fi

# Dump all names of images used in the test
docker exec -i "${TEST_NODE_ID}" journalctl -xu containerd > "${LOG_TMP}"
cat "${LOG_TMP}" | grep PullImage | sed -E 's/.*PullImage \\"([^\\]*)\\".*/\1/g' > "${LIST_TMP}"
cat "${LOG_TMP}" | grep SandboxImage | sed -E 's/.*SandboxImage:([^ ]*).*/\1/g' >> "${LIST_TMP}" || true
echo ${PAUSE_IMAGE_NAME} >> "${LIST_TMP}"
cat "${LIST_TMP}" | sort | uniq > "${IMAGE_LIST}"

docker kill "${TEST_NODE_ID}"
if [ "${FAIL}" != "" ] ; then
    exit 1
fi

exit 0
