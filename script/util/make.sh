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
IMAGE_NAME="minienv"

TMP_CONTEXT=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${TMP_CONTEXT}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM golang:1.15
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse
EOF
docker build -t "${IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"
docker run --rm --privileged \
       --device /dev/fuse \
       --tmpfs /tmp:exec,mode=777 \
       -w /go/src/github.com/containerd/stargz-snapshotter \
       -v "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro" \
       "${IMAGE_NAME}" make ${@} PREFIX=/tmp/out/
