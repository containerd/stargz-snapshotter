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
REGISTRY_HOST_AUTH="cri-registry-auth"
TEST_NODE_NAME="cri-testenv-container"
CRIO_SOCK=unix:///run/crio/crio.sock
PREPARE_NODE_NAME="cri-prepare-node"

source "${CONTEXT}/const.sh"
source "${REPO}/script/util/utils.sh"

IMAGE_LIST="${1}"

TMP_CONTEXT=$(mktemp -d)
DOCKER_COMPOSE_YAML=$(mktemp)
CRIO_CONFIG=$(mktemp)
STORE_CONFIG=$(mktemp)
TMPFILE=$(mktemp)
LOG_FILE=$(mktemp)
MIRROR_TMP=$(mktemp -d)
AUTH_DIR=$(mktemp -d)
DOCKERCONFIG=$(mktemp)
echo "${DOCKER_COMPOSE_YAML}"
function cleanup {
    ORG_EXIT_CODE="${1}"
    docker compose -f "${DOCKER_COMPOSE_YAML}" down -v || true
    rm -rf "${TMP_CONTEXT}" || true
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm "${CRIO_CONFIG}" || true
    rm "${STORE_CONFIG}" || true
    rm "${TMPFILE}" || true
    rm "${LOG_FILE}" || true
    rm -rf "${MIRROR_TMP}" || true
    rm -rf "${AUTH_DIR}" || true
    rm "${DOCKERCONFIG}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

RETRYNUM=100
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    local SUCCESS=false
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            SUCCESS=true
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ "${SUCCESS}" == "true" ] ; then
        return 0
    else
        return 1
    fi
}

DUMMYUSER=dummyuser
DUMMYPASS=dummypass
echo "Preparing creds..."
prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST_AUTH}" "${DUMMYUSER}" "${DUMMYPASS}"
echo -n '{"auths":{"'"${REGISTRY_HOST_AUTH}"':5000":{"auth":"'$(echo -n "${DUMMYUSER}:${DUMMYPASS}" | base64 -i -w 0)'"}}}' > "${DOCKERCONFIG}"

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
    - "critest-crio-data:/var/lib/containers"
    - "critest-crio-stargz-store-data:/var/lib/stargz-store"
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
    - TOOLS_DIR=/tools/
    volumes:
    - "critest-prepare-containerd-data:/var/lib/containerd"
    - "critest-prepare-containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc"
    - "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro"
    - "${MIRROR_TMP}:/tools/"
    - ${AUTH_DIR}:/auth
  registry:
    image: registry:2
    container_name: ${REGISTRY_HOST}
  registryauth:
    image: registry:2
    container_name: ${REGISTRY_HOST_AUTH}
    environment:
    - REGISTRY_AUTH=htpasswd
    - REGISTRY_AUTH_HTPASSWD_REALM="Registry Realm"
    - REGISTRY_AUTH_HTPASSWD_PATH=/auth/auth/htpasswd
    - REGISTRY_HTTP_TLS_CERTIFICATE=/auth/certs/domain.crt
    - REGISTRY_HTTP_TLS_KEY=/auth/certs/domain.key
    volumes:
    - ${AUTH_DIR}:/auth
volumes:
  critest-crio-data:
  critest-crio-stargz-store-data:
  critest-prepare-containerd-data:
  critest-prepare-containerd-stargz-grpc-data:
EOF
docker compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate

retry docker exec "${PREPARE_NODE_NAME}" curl -k --head "http://${REGISTRY_HOST}:5000/v2/"

# Mirror images used for auth test
docker exec "${PREPARE_NODE_NAME}" cp /auth/certs/domain.crt /usr/local/share/ca-certificates
docker exec "${PREPARE_NODE_NAME}" update-ca-certificates
docker exec "${PREPARE_NODE_NAME}" go install github.com/google/go-containerregistry/cmd/crane@latest
docker exec "${PREPARE_NODE_NAME}" mkdir /root/.docker/
docker cp "${DOCKERCONFIG}" "${PREPARE_NODE_NAME}:/root/.docker/config.json"
docker exec "${PREPARE_NODE_NAME}" crane copy ghcr.io/stargz-containers/ubuntu:22.04-esgz "${REGISTRY_HOST_AUTH}":5000/ubuntu:22.04-esgz

# Configure registry cert
docker exec "${TEST_NODE_NAME}" cp /auth/certs/domain.crt /usr/local/share/ca-certificates
docker exec "${TEST_NODE_NAME}" update-ca-certificates

# Mirror and optimize all images used in tests
echo "${REGISTRY_HOST}:5000" > "${MIRROR_TMP}/host"
cp "${IMAGE_LIST}" "${MIRROR_TMP}/list"
cp "${REPO}/script/cri-o/mirror.sh" "${MIRROR_TMP}/mirror.sh"
docker exec "${PREPARE_NODE_NAME}" /bin/bash /tools/mirror.sh

# Configure mirror registries for CRI-O and stargz store
docker exec "${TEST_NODE_NAME}" cat /etc/containers/registries.conf > "${CRIO_CONFIG}"
docker exec "${TEST_NODE_NAME}" cat /etc/stargz-store/config.toml > "${STORE_CONFIG}"
cat "${IMAGE_LIST}" | sed -E 's/^([^/]*).*/\1/g' | sort | uniq | while read DOMAIN ; do
    echo "Adding mirror config: ${DOMAIN}"
    cat <<EOF >> "${CRIO_CONFIG}"
[[registry]]
prefix = "${DOMAIN}"
insecure = true
blocked = false
location = "${REGISTRY_HOST}:5000"
EOF
    cat <<EOF >> "${STORE_CONFIG}"
[[resolver.host."${DOMAIN}".mirrors]]
host = "${REGISTRY_HOST}:5000"
insecure = true
EOF
done
echo "==== CRI-O (containers/image) config ===="
cat "${CRIO_CONFIG}"
echo "==== Store config ===="
cat "${STORE_CONFIG}"
docker cp "${CRIO_CONFIG}" "${TEST_NODE_NAME}":/etc/containers/registries.conf
docker cp "${STORE_CONFIG}" "${TEST_NODE_NAME}":/etc/stargz-store/config.toml

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
       "cd /go/src/github.com/kubernetes-sigs/cri-tools && make && make install -e BINDIR=/go/bin"

# Varidate the runtime through CRI
docker exec "${TEST_NODE_NAME}" systemctl restart stargz-store
docker exec "${TEST_NODE_NAME}" systemctl restart crio
CONNECTED=
for i in $(seq 100) ; do
    if docker exec "${TEST_NODE_NAME}" /go/bin/crictl --runtime-endpoint=${CRIO_SOCK} stats ; then
        CONNECTED=true
        break
    fi
    echo "Fail(${i}). Retrying..."
    sleep 1
done
if [ "${CONNECTED}" != "true" ] ; then
    echo "Failed to connect to CRI-O"
    exit 1
fi
echo "===== VERSION INFORMATION ====="
docker exec "${TEST_NODE_NAME}" runc --version
docker exec "${TEST_NODE_NAME}" crio --version
echo "==============================="

# Do auth test
docker exec "${TEST_NODE_NAME}" /go/bin/crictl --runtime-endpoint=${CRIO_SOCK} pull --creds "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST_AUTH}":5000/ubuntu:22.04-esgz

# Do CRI Validation test
docker exec "${TEST_NODE_NAME}" /go/bin/critest --runtime-endpoint=${CRIO_SOCK}

echo "Check all remote snapshots are created successfully"
docker exec "${TEST_NODE_NAME}" journalctl -u stargz-store \
    | grep "${LOG_REMOTE_SNAPSHOT}" \
    | sed -E 's/^[^\{]*(\{.*)$/\1/g' > "${LOG_FILE}"
check_remote_snapshots "${LOG_FILE}"

exit 0
