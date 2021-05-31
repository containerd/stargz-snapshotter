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
REGISTRY_HOST=k3s-private-registry
REGISTRY_NETWORK=k3s_registry_network
DUMMYUSER=dummyuser
DUMMYPASS=dummypass
TESTIMAGE_ORIGIN="ghcr.io/stargz-containers/ubuntu:20.04"
TESTIMAGE="${REGISTRY_HOST}:5000/library/ubuntu:20.04"
K3S_CLUSTER_NAME=k3s-stargz-snapshotter
PREPARE_NODE_NAME="cri-prepare-node"
PREPARE_NODE_IMAGE="cri-prepare-image"

source "${REPO}/script/util/utils.sh"

if [ "${K3S_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing preparation node image..."
    docker build ${DOCKER_BUILD_ARGS:-} -t "${PREPARE_NODE_IMAGE}" --target containerd-base "${REPO}"
fi

AUTH_DIR=$(mktemp -d)
DOCKERCONFIG=$(mktemp)
DOCKER_COMPOSE_YAML=$(mktemp)
K3S_KUBECONFIG=$(mktemp)
MIRROR_TMP=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${AUTH_DIR}" || true
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm "${DOCKERCONFIG}" || true
    rm "${K3S_KUBECONFIG}" || true
    rm -rf "${MIRROR_TMP}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

echo "Preparing creds..."
prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST}" "${DUMMYUSER}" "${DUMMYPASS}"
echo -n '{"auths":{"'"${REGISTRY_HOST}"':5000":{"auth":"'$(echo -n "${DUMMYUSER}:${DUMMYPASS}" | base64 -i -w 0)'"}}}' > "${DOCKERCONFIG}"

echo "Preparing private registry..."
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3.5"
services:
  testenv_registry:
    image: registry:2
    container_name: ${REGISTRY_HOST}
    environment:
    - HTTP_PROXY=${HTTP_PROXY:-}
    - HTTPS_PROXY=${HTTPS_PROXY:-}
    - http_proxy=${http_proxy:-}
    - https_proxy=${https_proxy:-}
    - REGISTRY_AUTH=htpasswd
    - REGISTRY_AUTH_HTPASSWD_REALM="Registry Realm"
    - REGISTRY_AUTH_HTPASSWD_PATH=/auth/auth/htpasswd
    - REGISTRY_HTTP_TLS_CERTIFICATE=/auth/certs/domain.crt
    - REGISTRY_HTTP_TLS_KEY=/auth/certs/domain.key
    volumes:
    - ${AUTH_DIR}:/auth
  image-prepare:
    image: "${PREPARE_NODE_IMAGE}"
    container_name: "${PREPARE_NODE_NAME}"
    privileged: true
    entrypoint:
    - sleep
    - infinity
    tmpfs:
    - /tmp:exec,mode=777
    environment:
    - REGISTRY_CREDS=${DUMMYUSER}:${DUMMYPASS}
    volumes:
    - "k3s-prepare-containerd-data:/var/lib/containerd"
    - "k3s-prepare-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
    - "${AUTH_DIR}/certs/domain.crt:/usr/local/share/ca-certificates/rgst.crt:ro"
    - "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro"
    - "${MIRROR_TMP}:/tools/"
volumes:
  k3s-prepare-containerd-data:
  k3s-prepare-containerd-stargz-grpc-data:
networks:
  default:
    external:
      name: ${REGISTRY_NETWORK}
EOF

cp "${REPO}/script/k3s/mirror.sh" "${MIRROR_TMP}/mirror.sh"
if ! ( cd "${CONTEXT}" && \
           docker network create "${REGISTRY_NETWORK}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
           docker exec "${PREPARE_NODE_NAME}" /bin/bash /tools/mirror.sh \
                  "${TESTIMAGE_ORIGIN}" "${TESTIMAGE}" ) ; then
    echo "Failed to prepare private registry"
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
    docker network rm "${REGISTRY_NETWORK}"
    exit 1
fi

echo "Testing in k3s cluster (kubeconfig: ${K3S_KUBECONFIG})..."
FAIL=
if ! ( "${CONTEXT}"/run-k3s.sh "${K3S_CLUSTER_NAME}" \
                 "${K3S_KUBECONFIG}" \
                 "${AUTH_DIR}/certs/domain.crt" \
                 "${REPO}" \
                 "${REGISTRY_NETWORK}" \
                 "${DOCKERCONFIG}" && \
         echo "Waiting until secrets fullly synced..." && \
         sleep 30 && \
         echo "Trying to pull private image with secret..." && \
         "${CONTEXT}"/create-pod.sh "$(k3d node list | grep ${K3S_CLUSTER_NAME}-server-0 | cut -d " " -f 1 | tr -d '\n')" \
                     "${K3S_KUBECONFIG}" "${TESTIMAGE}" ) ; then
    FAIL=true
fi
docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
k3d cluster delete "${K3S_CLUSTER_NAME}"
docker network rm "${REGISTRY_NETWORK}"

if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0
