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
REGISTRY_HOST=kind-private-registry
REGISTRY_NETWORK=kind_registry_network
DUMMYUSER=dummyuser
DUMMYPASS=dummypass
TESTIMAGE="${REGISTRY_HOST}:5000/library/ubuntu:18.04"
KIND_CLUSTER_NAME=kind-stargz-snapshotter

source "${REPO}/script/util/utils.sh"

AUTH_DIR=$(mktemp -d)
DOCKERCONFIG=$(mktemp)
DOCKER_COMPOSE_YAML=$(mktemp)
KIND_KUBECONFIG=$(mktemp)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${AUTH_DIR}" || true
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm "${DOCKERCONFIG}" || true
    rm "${KIND_KUBECONFIG}" || true
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
networks:
  default:
    external:
      name: ${REGISTRY_NETWORK}
EOF
if ! ( cd "${CONTEXT}" && \
           docker network create "${REGISTRY_NETWORK}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
           docker run --privileged --rm -i --network "${REGISTRY_NETWORK}" \
                  --device /dev/fuse \
                  --tmpfs "/tmp" \
                  -v "${AUTH_DIR}/certs/domain.crt:/usr/local/share/ca-certificates/rgst.crt:ro" \
                  -v "${DOCKERCONFIG}:/root/.docker/config.json:ro" \
                  -v "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro" \
                  golang:1.15-buster /bin/bash -c "apt-get update -y && \
apt-get --no-install-recommends install -y fuse && \
update-ca-certificates && \
cd /go/src/github.com/containerd/stargz-snapshotter && \
PREFIX=/out/ make ctr-remote && \
/out/ctr-remote images optimize ubuntu:18.04 ${TESTIMAGE}" ) ; then
    echo "Failed to prepare private registry"
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
    docker network rm "${REGISTRY_NETWORK}"
    exit 1
fi

echo "Testing in kind cluster (kubeconfig: ${KIND_KUBECONFIG})..."
FAIL=
if ! ( "${CONTEXT}"/run-kind.sh "${KIND_CLUSTER_NAME}" \
                 "${KIND_KUBECONFIG}" \
                 "${AUTH_DIR}/certs/domain.crt" \
                 "${REPO}" \
                 "${REGISTRY_NETWORK}" \
                 "${DOCKERCONFIG}" && \
         echo "Waiting until secrets fullly synced..." && \
         sleep 30 && \
         echo "Trying to pull private image with secret..." && \
         "${CONTEXT}"/create-pod.sh "$(kind get nodes --name "${KIND_CLUSTER_NAME}" | sed -n 1p)" \
                     "${KIND_KUBECONFIG}" "${TESTIMAGE}" ) ; then
    FAIL=true
fi
docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
kind delete cluster --name "${KIND_CLUSTER_NAME}"
docker network rm "${REGISTRY_NETWORK}"

if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0
