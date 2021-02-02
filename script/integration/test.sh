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

INTEGRATION_BASE_IMAGE_NAME="integration-image-base"
INTEGRATION_TEST_IMAGE_NAME="integration-image-test"
REGISTRY_HOST=registry-integration
REGISTRY_ALT_HOST=registry-alt
CONTAINERD_NODE=testenv_integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

source "${REPO}/script/util/utils.sh"

if [ "${INTEGRATION_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."

    # Enable to check race
    docker build ${DOCKER_BUILD_ARGS:-} -t "${INTEGRATION_BASE_IMAGE_NAME}" \
           --target snapshotter-base \
           --build-arg=SNAPSHOTTER_BUILD_FLAGS="-race" \
           "${REPO}"
fi

DOCKER_COMPOSE_YAML=$(mktemp)
AUTH_DIR=$(mktemp -d)
SS_ROOT_DIR=$(mktemp -d)
TMP_CONTEXT=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm -rf "${AUTH_DIR}" || true
    rm -rf "${SS_ROOT_DIR}" || true
    rm -rf "${TMP_CONTEXT}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

cp -R "${CONTEXT}/containerd" \
   "${REPO}/script/util/utils.sh" \
   "${TMP_CONTEXT}"
cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${INTEGRATION_BASE_IMAGE_NAME}

RUN apt-get update -y && \
    apt-get --no-install-recommends install -y iptables jq && \
    git clone https://github.com/google/go-containerregistry \
              \${GOPATH}/src/github.com/google/go-containerregistry && \
    cd \${GOPATH}/src/github.com/google/go-containerregistry && \
    git checkout eb7c14b719c60883de5747caa25463b44f8bf896 && \
    GO111MODULE=on go get github.com/google/go-containerregistry/cmd/crane && \
    git clone https://github.com/google/crfs \${GOPATH}/src/github.com/google/crfs && \
    cd \${GOPATH}/src/github.com/google/crfs && \
    git checkout 71d77da419c90be7b05d12e59945ac7a8c94a543 && \
    GO111MODULE=on go get github.com/google/crfs/stargz/stargzify

COPY ./containerd/config.containerd.toml /etc/containerd/config.toml
COPY ./containerd/config.stargz.toml /etc/containerd-stargz-grpc/config.toml
COPY ./containerd/entrypoint.sh ./utils.sh /

ENV CONTAINERD_SNAPSHOTTER=""

ENTRYPOINT [ "/entrypoint.sh" ]
EOF
docker build ${DOCKER_BUILD_ARGS:-} -t "${INTEGRATION_TEST_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

echo "Preparing creds..."
prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST}" "${DUMMYUSER}" "${DUMMYPASS}"

echo "Preparing docker-compose.yml..."
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3.3"
services:
  ${CONTAINERD_NODE}:
    image: ${INTEGRATION_TEST_IMAGE_NAME}
    container_name: testenv_integration
    privileged: true
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
    - "integration-containerd-data:/var/lib/containerd"
    - "integration-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
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

