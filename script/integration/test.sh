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
REGISTRY_HOST=registry-integration
REGISTRY_ALT_HOST=registry-alt
CONTAINERD_NODE=testenv_integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

source "${REPO}/script/util/utils.sh"

DOCKER_COMPOSE_YAML=$(mktemp)
AUTH_DIR=$(mktemp -d)
SS_ROOT_DIR=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm -rf "${AUTH_DIR}" || true
    rm -rf "${SS_ROOT_DIR}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

echo "Preparing creds..."
prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST}" "${DUMMYUSER}" "${DUMMYPASS}"

echo "Preparing docker-compose.yml..."
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3.3"
services:
  ${CONTAINERD_NODE}:
    build:
      context: "${CONTEXT}containerd"
      dockerfile: Dockerfile
    container_name: testenv_integration
    privileged: true
    working_dir: /go/src/github.com/containerd/stargz-snapshotter
    entrypoint: ./script/integration/containerd/entrypoint.sh
    environment:
    - NO_PROXY=127.0.0.1,localhost,${REGISTRY_HOST}:5000,${REGISTRY_ALT_HOST}:5000
    - HTTP_PROXY=${HTTP_PROXY:-}
    - HTTPS_PROXY=${HTTPS_PROXY:-}
    - http_proxy=${http_proxy:-}
    - https_proxy=${https_proxy:-}
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro"
    - ${AUTH_DIR}:/auth
    - /dev/fuse:/dev/fuse
    - type: volume
      source: integration-containerd-data
      target: /var/lib/containerd
      volume:
        nosuid: false
    - type: volume
      source: integration-containerd-stargz-grpc-data
      target: /var/lib/containerd-stargz-grpc
      volume:
        nosuid: false
    - type: volume
      source: integration-containerd-stargz-grpc-status
      target: /run/containerd-stargz-grpc
      volume:
        nosuid: false
  registry:
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
  registry-alt:
    image: registry:2
    container_name: registry-alt
volumes:
  integration-containerd-data:
  integration-containerd-stargz-grpc-data:
  integration-containerd-stargz-grpc-status:
EOF

echo "Testing..."
FAIL=
if ! ( cd "${CONTEXT}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} \
                          "${CONTAINERD_NODE}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" up --abort-on-container-exit ) ; then
    FAIL=true
fi
docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0

