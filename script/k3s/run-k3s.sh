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

K3S_VERSION=master
K3S_REPO=https://github.com/k3s-io/k3s
K3S_CONTAINERD_REPO=https://github.com/pdtpartners/containerd

REGISTRY_HOST=k3s-private-registry
K3S_NODE_REPO=ghcr.io/stargz-containers
K3S_NODE_IMAGE_NAME=k3s
K3S_NODE_TAG=1
K3S_NODE_IMAGE="${K3S_NODE_REPO}/${K3S_NODE_IMAGE_NAME}:${K3S_NODE_TAG}"

# Arguments
K3S_CLUSTER_NAME="${1}"
K3S_USER_KUBECONFIG="${2}"
K3S_REGISTRY_CA="${3}"
REPO="${4}"
REGISTRY_NETWORK="${5}"
DOCKERCONFIGJSON_DATA="${6}"

TMP_BUILTIN_CONF=$(mktemp)
TMP_CONTEXT=$(mktemp -d)
SN_KUBECONFIG=$(mktemp)
TMP_K3S_REPO=$(mktemp -d)
TMP_K3S_CONTAINERD_REPO=$(mktemp -d)
TMP_GOLANGCI=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${SN_KUBECONFIG}"
    rm -rf "${TMP_CONTEXT}"
    rm -rf "${TMP_BUILTIN_CONF}"
    rm -rf "${TMP_K3S_REPO}"
    rm -rf "${TMP_K3S_CONTAINERD_REPO}"
    rm "${TMP_GOLANGCI}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

echo "Preparing node image..."
git clone -b ${K3S_VERSION} --depth 1 "${K3S_REPO}" "${TMP_K3S_REPO}"
sed -i "s|github.com/k3s-io/stargz-snapshotter .*$|$(realpath ${REPO})|g" "${TMP_K3S_REPO}/go.mod"
sed -i "s|github.com/k3s-io/containerd v1.7.3-k3s1|github.com/pdtpartners/containerd v1.7.2-stargz|g" "${TMP_K3S_REPO}/go.mod"

echo "replace github.com/containerd/stargz-snapshotter/estargz => $(realpath ${REPO})/estargz" >> "${TMP_K3S_REPO}/go.mod"

# typeurl version stargz-snapshotter indirectly depends on is incompatible to the one github.com/k3s-io/containerd depends on.
# We use older version of typeurl which the both of the above are compatible to.
# We can remove this directive once k3s upgrades typeurl version to newer than v1.0.3-0.20220324183432-6193a0e03259.
echo "replace github.com/containerd/typeurl => github.com/containerd/typeurl v1.0.2" >> "${TMP_K3S_REPO}/go.mod"

cat "${TMP_K3S_REPO}/go.mod"

sed -i -E 's|(ENV DAPPER_RUN_ARGS .*)|\1 -v '"$(realpath ${REPO})":"$(realpath ${REPO})"':ro|g' "${TMP_K3S_REPO}/Dockerfile.dapper"
sed -i -E 's|(ENV DAPPER_ENV .*)|\1 DOCKER_BUILDKIT|g' "${TMP_K3S_REPO}/Dockerfile.dapper"
sed -i -E 's|github.com/k3s-io/containerd|github.com/pdtpartners/containerd|g' "${TMP_K3S_REPO}/scripts/download"

(
    cd "${TMP_K3S_REPO}" && \
        git config user.email "dummy@example.com" && \
        git config user.name "dummy" && \
        cat ./.golangci.json | jq '.run.deadline|="10m"' > "${TMP_GOLANGCI}" && \
        cp "${TMP_GOLANGCI}" ./.golangci.json &&  \
        go mod tidy && \
        make deps && \
        git add . && \
        git commit -m tmp && \
        REPO="${K3S_NODE_REPO}" IMAGE_NAME="${K3S_NODE_IMAGE_NAME}" TAG="${K3S_NODE_TAG}" SKIP_VALIDATE=1 make
)
cat <<EOF > "${TMP_BUILTIN_CONF}"
configs:
  ${REGISTRY_HOST}:5000:
    tls:
      ca_file: /registry.crt
EOF

echo "Createing k3s cluster"
k3d cluster create "${K3S_CLUSTER_NAME}" --image="${K3S_NODE_IMAGE}" \
    --registry-config="${TMP_BUILTIN_CONF}" -v "${K3S_REGISTRY_CA}":/registry.crt:ro \
    --k3s-arg='--snapshotter=stargz@server:*;agent:*'
k3d kubeconfig get "${K3S_CLUSTER_NAME}" > "${K3S_USER_KUBECONFIG}"
K3S_NODENAME="$(k3d node list | grep ${K3S_CLUSTER_NAME}-server-0 | cut -d " " -f 1 | tr -d '\n')"
docker network connect "${REGISTRY_NETWORK}" "${K3S_NODENAME}"

echo "Configuring kubernetes cluster..."
CONFIGJSON_BASE64="$(cat ${DOCKERCONFIGJSON_DATA} | base64 -i -w 0)"
cat <<EOF | KUBECONFIG="${K3S_USER_KUBECONFIG}" kubectl apply -f -
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
