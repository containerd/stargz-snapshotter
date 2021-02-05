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

NODE_IMAGE_NAME="stargz-snapshotter-node:1"
NODE_BASE_IMAGE_NAME="stargz-snapshotter-node-base:1"
NODE_TEST_CERT_FILE="/usr/local/share/ca-certificates/registry.crt"
SNAPSHOTTER_KUBECONFIG_PATH=/etc/kubernetes/snapshotter/config.conf
REGISTRY_HOST=kind-private-registry

# Arguments
KIND_CLUSTER_NAME="${1}"
KIND_USER_KUBECONFIG="${2}"
KIND_REGISTRY_CA="${3}"
REPO="${4}"
REGISTRY_NETWORK="${5}"
DOCKERCONFIGJSON_DATA="${6}"

TMP_CONTEXT=$(mktemp -d)
SN_KUBECONFIG=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${SN_KUBECONFIG}"
    rm -rf "${TMP_CONTEXT}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

if [ "${KIND_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."
    docker build ${DOCKER_BUILD_ARGS:-} -t "${NODE_BASE_IMAGE_NAME}" "${REPO}"
fi

# Prepare the testing node with enabling k8s keychain
cat <<'EOF' > "${TMP_CONTEXT}/config.stargz.append.toml"
[kubeconfig_keychain]
enable_keychain = true
kubeconfig_path = "/etc/kubernetes/snapshotter/config.conf"
EOF
cat <<EOF > "${TMP_CONTEXT}/config.containerd.append.toml"
[plugins."io.containerd.grpc.v1.cri".registry.configs."${REGISTRY_HOST}:5000".tls]
ca_file = "${NODE_TEST_CERT_FILE}"
EOF
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${NODE_BASE_IMAGE_NAME}

COPY ./config.stargz.append.toml ./config.containerd.append.toml /tmp/
RUN cat /tmp/config.stargz.append.toml >> /etc/containerd-stargz-grpc/config.toml && \
    cat /tmp/config.containerd.append.toml >> /etc/containerd/config.toml
EOF
docker build -t "${NODE_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

# cluster must be single node
echo "Cleating kind cluster and connecting to the registry network..."
kind create cluster --name "${KIND_CLUSTER_NAME}" \
     --kubeconfig "${KIND_USER_KUBECONFIG}" \
     --image "${NODE_IMAGE_NAME}"
KIND_NODENAME=$(kind get nodes --name "${KIND_CLUSTER_NAME}" | sed -n 1p) # must be single node
docker network connect "${REGISTRY_NETWORK}" "${KIND_NODENAME}"
docker cp "${KIND_REGISTRY_CA}" "${KIND_NODENAME}:${NODE_TEST_CERT_FILE}"
docker exec -i "${KIND_NODENAME}" update-ca-certificates
docker exec -i "${KIND_NODENAME}" systemctl restart stargz-snapshotter

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
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: stargz-snapshotter
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: stargz-snapshotter
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: stargz-snapshotter
subjects:
- kind: ServiceAccount
  name: stargz-snapshotter
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: stargz-snapshotter
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list", "watch"]
EOF

echo "Installing kubeconfig for stargz snapshotter on node...."
APISERVER_PORT=$(docker exec -i "${KIND_NODENAME}" ps axww \
                     | grep kube-apiserver | sed -n -E 's/.*--secure-port=([0-9]*).*/\1/p')
TOKENNAME=$(KUBECONFIG="${KIND_USER_KUBECONFIG}" kubectl get sa stargz-snapshotter -o jsonpath='{.secrets[0].name}')
CA=$(KUBECONFIG="${KIND_USER_KUBECONFIG}" kubectl get secret/${TOKENNAME} -o jsonpath='{.data.ca\.crt}')
TOKEN=$(KUBECONFIG="${KIND_USER_KUBECONFIG}" kubectl get secret/${TOKENNAME} -o jsonpath='{.data.token}' \
            | base64 --decode)
cat <<EOF > "${SN_KUBECONFIG}"
apiVersion: v1
kind: Config
clusters:
- name: default-cluster
  cluster:
    certificate-authority-data: ${CA}
    server: https://${KIND_NODENAME}:${APISERVER_PORT}
contexts:
- name: default-context
  context:
    cluster: default-cluster
    namespace: default
    user: default-user
current-context: default-context
users:
- name: default-user
  user:
    token: ${TOKEN}
EOF
docker exec -i "${KIND_NODENAME}" mkdir -p $(dirname "${SNAPSHOTTER_KUBECONFIG_PATH}")
docker cp "${SN_KUBECONFIG}" "${KIND_NODENAME}:${SNAPSHOTTER_KUBECONFIG_PATH}"
