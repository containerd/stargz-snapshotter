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
REGISTRY_HOST=registry-integration.test
REGISTRY_ALT_HOST=registry-alt.test
CONTAINERD_NODE=testenv_integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

IPFS_VERSION=v0.17.0

source "${REPO}/script/util/utils.sh"

if [ "${INTEGRATION_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."

    TARGET_STAGE=snapshotter-base
    if [ "${BUILTIN_SNAPSHOTTER:-}" == "true" ] ; then
        TARGET_STAGE=containerd-snapshotter-base
    fi

    # Enable to check race
    # Temporary disable race check until docker/cli fixing race issue
    # https://github.com/docker/cli/pull/3264
    # BUILD_FLAGS="-race"
    BUILD_FLAGS=""
    docker build ${DOCKER_BUILD_ARGS:-} -t "${INTEGRATION_BASE_IMAGE_NAME}" \
           --target "${TARGET_STAGE}" \
           --build-arg=SNAPSHOTTER_BUILD_FLAGS="${BUILD_FLAGS}" \
           "${REPO}"
fi

USE_METADATA_STORE="memory"
if [ "${METADATA_STORE:-}" != "" ] ; then
    USE_METADATA_STORE="${METADATA_STORE}"
fi

SNAPSHOTTER_CONFIG_FILE=/etc/containerd-stargz-grpc/config.toml
if [ "${BUILTIN_SNAPSHOTTER:-}" == "true" ] ; then
    SNAPSHOTTER_CONFIG_FILE=/etc/containerd/config.toml
fi

USE_FUSE_PASSTHROUGH="false"
if [ "${FUSE_PASSTHROUGH:-}" != "" ] ; then
    USE_FUSE_PASSTHROUGH="${FUSE_PASSTHROUGH}"
    if [ "${BUILTIN_SNAPSHOTTER:-}" == "true" ] && [ "${FUSE_PASSTHROUGH}" == "true" ] ; then
        echo "builtin snapshotter + fuse passthrough test is unsupported"
        exit 1
    fi
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
    apt-get --no-install-recommends install -y iptables jq netcat && \
    go install github.com/google/crfs/stargz/stargzify@71d77da419c90be7b05d12e59945ac7a8c94a543 && \
    wget https://dist.ipfs.io/go-ipfs/${IPFS_VERSION}/go-ipfs_${IPFS_VERSION}_linux-amd64.tar.gz && \
    tar -xvzf go-ipfs_${IPFS_VERSION}_linux-amd64.tar.gz && \
    cd go-ipfs && \
    bash install.sh

COPY ./containerd/config.containerd.toml /etc/containerd/config.toml
COPY ./containerd/config.stargz.toml /etc/containerd-stargz-grpc/config.toml
COPY ./containerd/entrypoint.sh ./utils.sh /

RUN if [ "${BUILTIN_SNAPSHOTTER:-}" != "true" ] ; then \
      sed -i 's/^metadata_store.*/metadata_store = "${USE_METADATA_STORE}"/g' "${SNAPSHOTTER_CONFIG_FILE}" && \
      sed -i 's/^passthrough.*/passthrough = ${USE_FUSE_PASSTHROUGH}/g' "${SNAPSHOTTER_CONFIG_FILE}" ; \
    fi

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
    - NO_PROXY=127.0.0.1,localhost,${REGISTRY_HOST}:443,${REGISTRY_ALT_HOST}:5000
    - HTTP_PROXY=${HTTP_PROXY:-}
    - HTTPS_PROXY=${HTTPS_PROXY:-}
    - http_proxy=${http_proxy:-}
    - https_proxy=${https_proxy:-}
    - BUILTIN_SNAPSHOTTER=${BUILTIN_SNAPSHOTTER:-}
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
    - REGISTRY_HTTP_ADDR=${REGISTRY_HOST}:443
    volumes:
    - ${AUTH_DIR}:/auth
  registry-alt:
    image: registry:2
    container_name: "${REGISTRY_ALT_HOST}"
volumes:
  integration-containerd-data:
  integration-containerd-stargz-grpc-data:
EOF

echo "Testing..."
FAIL=
if ! ( cd "${CONTEXT}" && \
           docker compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} \
                          "${CONTAINERD_NODE}" && \
           docker compose -f "${DOCKER_COMPOSE_YAML}" up --abort-on-container-exit ) ; then
    FAIL=true
fi
docker compose -f "${DOCKER_COMPOSE_YAML}" down -v
if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0

