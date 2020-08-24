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

cleanup() {
    local ORG_EXIT_CODE="${1}"
    rm "${DOCKER_COMPOSE_YAML}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

CONTEXT="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/"
REPO="${CONTEXT}../../"

echo "Preparing docker-compose.yml..."
DOCKER_COMPOSE_YAML=$(mktemp)
BENCHMARKING_NODE=fs-bench
BENCHMARKING_CONTAINER=fs-bench
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3"
services:
  ${BENCHMARKING_NODE}:
    build:
      context: "${CONTEXT}/work"
      dockerfile: Dockerfile
    container_name: ${BENCHMARKING_CONTAINER}
    privileged: true
    working_dir: /go/src/github.com/containerd/stargz-snapshotter
    command: tail -f /dev/null
    environment:
    - NO_PROXY=127.0.0.1,localhost
    - HTTP_PROXY=${HTTP_PROXY:-}
    - HTTPS_PROXY=${HTTPS_PROXY:-}
    - http_proxy=${http_proxy:-}
    - https_proxy=${https_proxy:-}
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro"
    - "/dev/fuse:/dev/fuse"
    - "containerd-data:/var/lib/containerd:delegated"
    - "containerd-stargz-grpc-data:/var/lib/containerd-stargz-grpc:delegated"
    - "containerd-stargz-grpc-status:/run/containerd-stargz-grpc:delegated"
volumes:
  containerd-data:
  containerd-stargz-grpc-data:
  containerd-stargz-grpc-status:
EOF

echo "Setup benchmark environment..."
docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} "${BENCHMARKING_NODE}"
docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate

echo "Benchmarking..."
docker exec -i "${BENCHMARKING_CONTAINER}" script/fs-bench/work/run.sh

echo "Harvesting output..."
docker cp "${BENCHMARKING_CONTAINER}:/output" "${REPO}/out-bench"

echo "Cleaning up benchmark environment..."
docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v