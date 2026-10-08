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

NODE_IMAGE_NAME="cri-stargz-snapshotter-node:1"
NODE_BASE_IMAGE_NAME="cri-stargz-snapshotter-node-base:1"
NODE_TEST_CERT_FILE="/usr/local/share/ca-certificates/registry.crt"
REGISTRY_HOST=kind-cri-private-registry

# Arguments
KIND_CLUSTER_NAME="${1}"
KIND_USER_KUBECONFIG="${2}"
KIND_REGISTRY_CA="${3}"
REPO="${4}"
REGISTRY_NETWORK="${5}"
DOCKERCONFIGJSON_DATA="${6}"

TMP_BUILTIN_CONF=$(mktemp)
TMP_CONTEXT=$(mktemp -d)
SN_KUBECONFIG=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${SN_KUBECONFIG}"
    rm -rf "${TMP_CONTEXT}"
    rm -rf "${TMP_BUILTIN_CONF}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

if [ "${KIND_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."

    TARGET_STAGE=
    if [ "${BUILTIN_SNAPSHOTTER:-}" == "true" ] ; then
        TARGET_STAGE="--target kind-builtin-snapshotter"
    fi

    docker build ${DOCKER_BUILD_ARGS:-} -t "${NODE_BASE_IMAGE_NAME}" ${TARGET_STAGE} "${REPO}"
fi

# Prepare the testing node with enabling k8s keychain
cat <<EOF > "${TMP_CONTEXT}/config.containerd.append.toml"
[plugins."io.containerd.grpc.v1.cri".registry]
config_path = "/etc/containerd/certs.d"
EOF
cat <<EOF > "${TMP_CONTEXT}/registry-hosts.toml"
server = "https://${REGISTRY_HOST}:5000"
[host."${REGISTRY_HOST}:5000"]
  ca = "${NODE_TEST_CERT_FILE}"
EOF
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
[plugins."io.containerd.grpc.v1.cri".registry.configs."${REGISTRY_HOST}:5000".tls]
ca_file = "${NODE_TEST_CERT_FILE}"
[plugins."io.containerd.snapshotter.v1.stargz"]
cri_keychain_image_service_path = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
[plugins."io.containerd.snapshotter.v1.stargz".cri_keychain]
enable_keychain = true
[plugins."io.containerd.snapshotter.v1.stargz".registry.configs."${REGISTRY_HOST}:5000".tls]
ca_file = "${NODE_TEST_CERT_FILE}"
EOF
    BUILTIN_HACK_INST="COPY containerd.hack.toml /etc/containerd/config.toml"
fi

cp "${KIND_REGISTRY_CA}" "${TMP_CONTEXT}/registry.crt"

if [ "${FUSE_MANAGER:-}" == "true" ] ; then
    echo "Enabling fuse manager"
    cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${NODE_BASE_IMAGE_NAME}

COPY registry.crt "${NODE_TEST_CERT_FILE}"
COPY ./config.containerd.append.toml /tmp/
COPY ./registry-hosts.toml /etc/containerd/certs.d/${REGISTRY_HOST}:5000/hosts.toml
RUN cat /tmp/config.containerd.append.toml >> /etc/containerd/config.toml && \
    update-ca-certificates

RUN sed -i '1icri_listen_path = "/run/containerd-stargz-grpc/fuse-manager-cri.sock"' /etc/containerd-stargz-grpc/config.toml
RUN echo "[fuse_manager]" >> /etc/containerd-stargz-grpc/config.toml
RUN echo "enable = true" >> /etc/containerd-stargz-grpc/config.toml
RUN echo "KUBELET_EXTRA_ARGS=--fail-swap-on=false --image-service-endpoint=unix:///run/containerd-stargz-grpc/fuse-manager-cri.sock" > /etc/default/kubelet

EOF

else
    cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${NODE_BASE_IMAGE_NAME}

COPY registry.crt "${NODE_TEST_CERT_FILE}"
COPY ./config.containerd.append.toml /tmp/
COPY ./registry-hosts.toml /etc/containerd/certs.d/${REGISTRY_HOST}:5000/hosts.toml
RUN cat /tmp/config.containerd.append.toml >> /etc/containerd/config.toml && \
    update-ca-certificates

${BUILTIN_HACK_INST}

EOF
fi

if [ "${TRANSFER_SERVICE:-}" == "false" ] ; then
    cat <<'EOF' >> "${TMP_CONTEXT}/Dockerfile"
RUN sed -i '/\[plugins\."io\.containerd\.grpc\.v1\.cri"\.containerd\]/a\  disable_snapshot_annotations = false' /etc/containerd/config.toml
RUN cat /etc/containerd/config.toml
EOF
fi

docker build -t "${NODE_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

# cluster must be single node
echo "Cleating kind cluster and connecting to the registry network..."
kind create cluster --name "${KIND_CLUSTER_NAME}" \
     --kubeconfig "${KIND_USER_KUBECONFIG}" \
     --image "${NODE_IMAGE_NAME}"
KIND_NODENAME=$(kind get nodes --name "${KIND_CLUSTER_NAME}" | sed -n 1p) # must be single node
docker network connect "${REGISTRY_NETWORK}" "${KIND_NODENAME}"

echo "===== VERSION INFORMATION ====="
docker exec "${KIND_NODENAME}" containerd --version
docker exec "${KIND_NODENAME}" runc --version
echo "==============================="

echo "Configuring kubernetes cluster..."
CONFIGJSON_BASE64="$(cat ${DOCKERCONFIGJSON_DATA} | base64 -i -w 0)"
cat <<EOF | KUBECONFIG="${KIND_USER_KUBECONFIG}" kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: ns1
---
apiVersion: v1
kind: Secret
metadata:
  name: testsecret
  namespace: ns1
data:
  .dockerconfigjson: ${CONFIGJSON_BASE64}
type: kubernetes.io/dockerconfigjson
EOF
