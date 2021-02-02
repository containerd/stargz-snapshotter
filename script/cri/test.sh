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

source "${CONTEXT}/const.sh"

if [ "${CRI_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."
    docker build ${DOCKER_BUILD_ARGS:-} -t "${NODE_BASE_IMAGE_NAME}" "${REPO}"
    docker build ${DOCKER_BUILD_ARGS:-} -t "${PREPARE_NODE_IMAGE}" --target containerd-base "${REPO}"
fi

TMP_CONTEXT=$(mktemp -d)
IMAGE_LIST=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${TMP_CONTEXT}" || true
    rm "${IMAGE_LIST}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

# Prepare the testing node
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
# Legacy builder that doesn't support TARGETARCH should set this explicitly using --build-arg.
# If TARGETARCH isn't supported by the builder, the default value is "amd64".

FROM ${NODE_BASE_IMAGE_NAME}
ARG TARGETARCH

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
RUN apt install -y --no-install-recommends git make gcc build-essential jq && \
    curl https://dl.google.com/go/go1.15.6.linux-\${TARGETARCH:-amd64}.tar.gz \
    | tar -C /usr/local -xz && \
    go get -u github.com/onsi/ginkgo/ginkgo && \
    git clone -b v1.19.0 https://github.com/kubernetes-sigs/cri-tools \
              \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    cd \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    make critest && make install-critest -e BINDIR=\${GOPATH}/bin && \
    git clone -b v1.11.1 https://github.com/containerd/cri \
              \${GOPATH}/src/github.com/containerd/cri && \
    cd \${GOPATH}/src/github.com/containerd/cri && \
    NOSUDO=true ./hack/install/install-cni.sh && \
    NOSUDO=true ./hack/install/install-cni-config.sh && \
    systemctl disable kubelet

ENTRYPOINT [ "/usr/local/bin/entrypoint", "/sbin/init" ]
EOF
docker build -t "${NODE_TEST_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

echo "Testing..."
"${CONTEXT}/test-legacy.sh" "${IMAGE_LIST}"
"${CONTEXT}/test-stargz.sh" "${IMAGE_LIST}"
