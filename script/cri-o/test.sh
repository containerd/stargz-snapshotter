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
source "${REPO}/script/util/utils.sh"

CRI_TOOLS_VERSION=$(get_version_from_arg "${REPO}/Dockerfile" "CRI_TOOLS_VERSION")
GINKGO_VERSION=v1.16.5

if [ "${CRI_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."

    docker build ${DOCKER_BUILD_ARGS:-} -t "${NODE_BASE_IMAGE_NAME}" --target crio-stargz-store "${REPO}"
    docker build ${DOCKER_BUILD_ARGS:-} -t "${PREPARE_NODE_IMAGE}" --target containerd-base "${REPO}"
fi

USE_METADATA_STORE="memory"
if [ "${METADATA_STORE:-}" != "" ] ; then
    USE_METADATA_STORE="${METADATA_STORE}"
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

cat <<EOF > "${TMP_CONTEXT}/crio.conf"
[crio.runtime]
default_runtime = "runc"
[crio.runtime.runtimes.runc]
runtime_path = "/usr/local/sbin/runc"
EOF

# Prepare the testing node
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
# Legacy builder that doesn't support TARGETARCH should set this explicitly using --build-arg.
# If TARGETARCH isn't supported by the builder, the default value is "amd64".

FROM ${NODE_BASE_IMAGE_NAME}
ARG TARGETARCH

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
# Do not install git and its dependencies here which will cause failure of building the image
RUN apt-get update && apt-get install -y --no-install-recommends make && \
    curl -Ls https://dl.google.com/go/go1.23.0.linux-\${TARGETARCH:-amd64}.tar.gz | tar -C /usr/local -xz && \
    go install github.com/onsi/ginkgo/ginkgo@${GINKGO_VERSION} && \
    mkdir -p \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools /tmp/cri-tools && \
    curl -sL https://github.com/kubernetes-sigs/cri-tools/archive/refs/tags/v${CRI_TOOLS_VERSION}.tar.gz | tar -C /tmp/cri-tools -xz && \
    mv /tmp/cri-tools/cri-tools-${CRI_TOOLS_VERSION}/* \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools/ && \
    cd \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    make && make install -e BINDIR=\${GOPATH}/bin

RUN echo "metadata_store = \"${USE_METADATA_STORE}\"" >> /etc/stargz-store/config.toml

COPY ./crio.conf /etc/crio/

ENTRYPOINT [ "/usr/local/bin/entrypoint" ]
EOF
docker build -t "${NODE_TEST_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

echo "Testing..."
"${CONTEXT}/test-legacy.sh" "${IMAGE_LIST}"
"${CONTEXT}/test-stargz.sh" "${IMAGE_LIST}"
