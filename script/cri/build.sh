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

NODE_TEST_IMAGE_NAME="cri-integration-node-testimage"
NODE_BASE_IMAGE_NAME="cri-integration-node-baseimage"

REPO="${1}"

if [ "${CRI_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."
    docker build -t "${NODE_BASE_IMAGE_NAME}" "${REPO}"
fi

TMP_CONTEXT=$(mktemp -d)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm -rf "${TMP_CONTEXT}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

# Prepare the testing node
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${NODE_BASE_IMAGE_NAME}

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
RUN apt install -y --no-install-recommends git make gcc build-essential && \
    curl https://dl.google.com/go/go1.13.9.linux-amd64.tar.gz \
    | tar -C /usr/local -xz && \
    go get -u github.com/onsi/ginkgo/ginkgo && \
    git clone -b v1.18.0 https://github.com/kubernetes-sigs/cri-tools \
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
docker build -t "${NODE_TEST_IMAGE_NAME}" "${TMP_CONTEXT}"
