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

REGISTRY_HOST="cri-registry"
TEST_NODE_NAME="cri-testenv-container"
CONTAINERD_SOCK=unix:///run/containerd/containerd.sock
PREPARE_NODE_NAME="cri-prepare-node"

source "${CONTEXT}/const.sh"
source "${REPO}/script/util/utils.sh"

IMAGE_LIST="${1}"

TMP_CONTEXT=$(mktemp -d)
DOCKER_COMPOSE_YAML=$(mktemp)
CONTAINERD_CONFIG=$(mktemp)
SNAPSHOTTER_CONFIG=$(mktemp)
TMPFILE=$(mktemp)
LOG_FILE=$(mktemp)
MIRROR_TMP=$(mktemp -d)
function cleanup {
    ORG_EXIT_CODE="${1}"
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v || true
    rm -rf "${TMP_CONTEXT}" || true
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm "${CONTAINERD_CONFIG}" || true
    rm "${SNAPSHOTTER_CONFIG}" || true
    rm "${TMPFILE}" || true
    rm "${LOG_FILE}" || true
    rm -rf "${MIRROR_TMP}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

# Prepare the testing node and registry
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3.3"
services:
  cri-testenv-service:
    image: ${NODE_TEST_IMAGE_NAME}
    container_name: ${TEST_NODE_NAME}
    privileged: true
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - /dev/fuse:/dev/fuse
    - "critest-containerd-data:/var/lib/containerd"
    - "critest-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
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
    - TOOLS_DIR=/tools/
    volumes:
    - "critest-prepare-containerd-data:/var/lib/containerd"
    - "critest-prepare-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
    - "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro"
    - "${MIRROR_TMP}:/tools/"
  registry:
    image: registry:2
    container_name: ${REGISTRY_HOST}
volumes:
  critest-containerd-data:
  critest-containerd-stargz-grpc-data:
  critest-prepare-containerd-data:
  critest-prepare-containerd-stargz-grpc-data:
EOF
docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate

CONNECTED=
for i in $(seq 100) ; do
    if docker exec "${TEST_NODE_NAME}" curl -k --head "http://${REGISTRY_HOST}:5000/v2/" ; then
        CONNECTED=true
        break
    fi
    echo "Fail(${i}). Retrying..."
    sleep 1
done
if [ "${CONNECTED}" != "true" ] ; then
    echo "Failed to connect to containerd"
    exit 1
fi

# Mirror and optimize all images used in tests
echo "${REGISTRY_HOST}:5000" > "${MIRROR_TMP}/host"
cp "${IMAGE_LIST}" "${MIRROR_TMP}/list"
cp "${REPO}/script/cri/mirror.sh" "${MIRROR_TMP}/mirror.sh"
docker exec "${PREPARE_NODE_NAME}" /bin/bash /tools/mirror.sh

# Configure mirror registries for containerd and snapshotter
docker exec "${TEST_NODE_NAME}" cat /etc/containerd/config.toml > "${CONTAINERD_CONFIG}"
docker exec "${TEST_NODE_NAME}" cat /etc/containerd-stargz-grpc/config.toml > "${SNAPSHOTTER_CONFIG}"
cat "${IMAGE_LIST}" | sed -E 's/^([^/]*).*/\1/g' | sort | uniq | while read DOMAIN ; do
    echo "Adding mirror config: ${DOMAIN}"
    cat <<EOF >> "${CONTAINERD_CONFIG}"
[plugins."io.containerd.grpc.v1.cri".registry.mirrors."${DOMAIN}"]
endpoint = ["http://${REGISTRY_HOST}:5000"]
EOF
    cat <<EOF >> "${SNAPSHOTTER_CONFIG}"
[[resolver.host."${DOMAIN}".mirrors]]
host = "${REGISTRY_HOST}:5000"
insecure = true
EOF
done
echo "==== Containerd config ===="
cat "${CONTAINERD_CONFIG}"
echo "==== Snapshotter config ===="
cat "${SNAPSHOTTER_CONFIG}"
docker cp "${CONTAINERD_CONFIG}" "${TEST_NODE_NAME}":/etc/containerd/config.toml
docker cp "${SNAPSHOTTER_CONFIG}" "${TEST_NODE_NAME}":/etc/containerd-stargz-grpc/config.toml

# Replace digests specified in testing tool to stargz-formatted one
docker exec "${PREPARE_NODE_NAME}" ctr-remote i ls
cat "${IMAGE_LIST}" | grep "@sha256:" | while read IMAGE ; do
    URL_PATH=$(echo "${IMAGE}" | sed -E 's/^[^/]*//g' | sed -E 's/@.*//g')
    MIRROR_TAG="${REGISTRY_HOST}:5000${URL_PATH}"
    OLD_DIGEST=$(echo "${IMAGE}" | sed -E 's/.*(sha256:[a-z0-9]*).*/\1/g')
    echo "Getting the digest of : ${MIRROR_TAG}"
    NEW_DIGEST=$(docker exec "${PREPARE_NODE_NAME}" ctr-remote i ls name=="${MIRROR_TAG}" \
                     | grep "sha256" | sed -E 's/.*(sha256:[a-z0-9]*).*/\1/g')
    echo "Converting: ${OLD_DIGEST} => ${NEW_DIGEST}"
    docker exec "${TEST_NODE_NAME}" \
           find /go/src/github.com/kubernetes-sigs/cri-tools/pkg -type f -exec \
           sed -i -e "s|${OLD_DIGEST}|${NEW_DIGEST}|g" {} \;
done

# Rebuild cri testing tool
docker exec "${TEST_NODE_NAME}" /bin/bash -c \
       "cd /go/src/github.com/kubernetes-sigs/cri-tools && make critest && make install-critest -e BINDIR=/go/bin"

# Varidate the runtime through CRI
docker exec "${TEST_NODE_NAME}" systemctl restart stargz-snapshotter
docker exec "${TEST_NODE_NAME}" systemctl restart containerd
CONNECTED=
for i in $(seq 100) ; do
    if docker exec "${TEST_NODE_NAME}" ctr version ; then
        CONNECTED=true
        break
    fi
    echo "Fail(${i}). Retrying..."
    sleep 1
done
if [ "${CONNECTED}" != "true" ] ; then
    echo "Failed to connect to containerd"
    exit 1
fi
echo "===== VERSION INFORMATION ====="
docker exec "${TEST_NODE_NAME}" runc --version
docker exec "${TEST_NODE_NAME}" containerd --version
echo "==============================="
docker exec "${TEST_NODE_NAME}" /go/bin/critest --runtime-endpoint=${CONTAINERD_SOCK}

# Check if stargz snapshotter is working
docker exec "${TEST_NODE_NAME}" \
       ctr-remote --namespace=k8s.io snapshot --snapshotter=stargz ls \
    | sed -E '1d' > "${TMPFILE}"
if ! [ -s "${TMPFILE}" ] ; then
    echo "No snapshots created; stargz snapshotter might be connected to containerd"
    exit 1
fi

# Check all remote snapshots are created successfully
docker exec "${TEST_NODE_NAME}" journalctl -u stargz-snapshotter \
    | grep "${LOG_REMOTE_SNAPSHOT}" \
    | sed -E 's/^[^\{]*(\{.*)$/\1/g' > "${LOG_FILE}"
check_remote_snapshots "${LOG_FILE}"

exit 0
