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

source "${REPO}/script/util/utils.sh"

NODE_IMAGE_NAME_BASE=test-podman-rootless-node-base
NODE_IMAGE_NAME=test-podman-rootless-node

if [ "${PODMAN_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."
    docker build ${DOCKER_BUILD_ARGS:-} --progress=plain -t "${NODE_IMAGE_NAME_BASE}" --target podman-rootless "${REPO}"
fi

TEST_LOG=$(mktemp)
STORE_LOG=$(mktemp)
TMP_CONTEXT=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${TMP_CONTEXT}" || true
    rm "${TEST_LOG}" || true
    rm "${STORE_LOG}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

cp "${CONTEXT}/run_test.sh" "${TMP_CONTEXT}/run_test.sh"
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${NODE_IMAGE_NAME_BASE}

COPY run_test.sh /run_test.sh
CMD ["/bin/bash", "--login", "/run_test.sh"]
EOF

docker build ${DOCKER_BUILD_ARGS:-} --progress=plain -t "${NODE_IMAGE_NAME}" "${TMP_CONTEXT}"
docker run -t --rm --privileged "${NODE_IMAGE_NAME}" | tee "${TEST_LOG}"
cat "${TEST_LOG}" | grep "${LOG_REMOTE_SNAPSHOT}" | sed -E 's/^[^\{]*(\{.*)$/\1/g' > "${STORE_LOG}"
check_remote_snapshots "${STORE_LOG}"
