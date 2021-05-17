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
CRI_TOOLS_VERSION=53ad8bb7f97e1b1d1c0c0634e43a3c2b8b07b718
CNI_VERSION="v0.9.1"
SNAPSHOTTER_SOCK_PATH=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock

source "${CONTEXT}/const.sh"

if [ "${CRI_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."

    TARGET_STAGE=
    if [ "${BUILTIN_SNAPSHOTTER:-}" == "true" ] ; then
        TARGET_STAGE="--target kind-builtin-snapshotter"
    fi

    docker build ${DOCKER_BUILD_ARGS:-} -t "${NODE_BASE_IMAGE_NAME}" ${TARGET_STAGE} "${REPO}"
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

BUILTIN_HACK_INST=
if [ "${BUILTIN_SNAPSHOTTER:-}" == "true" ] ; then
    # Special configuration for CRI containerd + builtin stargz snapshotter
    cat <<EOF > "${TMP_CONTEXT}/containerd.hack.toml"
version = 2

[debug]
  format = "json"
  level = "debug"
[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "runc"
  snapshotter = "stargz"
  disable_snapshot_annotations = false
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.test-handler]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.snapshotter.v1.stargz"]
cri_keychain_image_service_path = "${SNAPSHOTTER_SOCK_PATH}"
[plugins."io.containerd.snapshotter.v1.stargz".cri_keychain]
enable_keychain = true
EOF
    BUILTIN_HACK_INST="COPY containerd.hack.toml /etc/containerd/config.toml"
fi

cat <<EOF > "${TMP_CONTEXT}/test.conflist"
{
  "cniVersion": "0.4.0",
  "name": "containerd-net",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni0",
      "isGateway": true,
      "ipMasq": true,
      "promiscMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [
          [{
            "subnet": "10.88.0.0/16"
          }]
        ],
        "routes": [
          { "dst": "0.0.0.0/0" }
        ]
      }
    },
    {
      "type": "portmap",
      "capabilities": {"portMappings": true}
    }
  ]
}
EOF

# Prepare the testing node
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
# Legacy builder that doesn't support TARGETARCH should set this explicitly using --build-arg.
# If TARGETARCH isn't supported by the builder, the default value is "amd64".

FROM ${NODE_BASE_IMAGE_NAME}
ARG TARGETARCH

ENV PATH=$PATH:/usr/local/go/bin
ENV GOPATH=/go
RUN apt install -y --no-install-recommends git make gcc build-essential jq && \
    curl https://dl.google.com/go/go1.16.4.linux-\${TARGETARCH:-amd64}.tar.gz \
    | tar -C /usr/local -xz && \
    go get -u github.com/onsi/ginkgo/ginkgo && \
    git clone https://github.com/kubernetes-sigs/cri-tools \
              \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    cd \${GOPATH}/src/github.com/kubernetes-sigs/cri-tools && \
    git checkout ${CRI_TOOLS_VERSION} && \
    make && make install -e BINDIR=\${GOPATH}/bin && \
    curl -Ls https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-\${TARGETARCH:-amd64}-${CNI_VERSION}.tgz | tar xzv -C /opt/cni/bin && \
    systemctl disable kubelet

COPY ./test.conflist /etc/cni/net.d/test.conflist

${BUILTIN_HACK_INST}

ENTRYPOINT [ "/usr/local/bin/entrypoint", "/sbin/init" ]
EOF
docker build -t "${NODE_TEST_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

echo "Testing..."
"${CONTEXT}/test-legacy.sh" "${IMAGE_LIST}"
"${CONTEXT}/test-stargz.sh" "${IMAGE_LIST}"
