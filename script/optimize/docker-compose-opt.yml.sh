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

REGISTRY_HOST=registry-optimize
REPO_PATH=/go/src/github.com/containerd/stargz-snapshotter

if [ "${1}" == "" ]; then
    echo "Repository path must be provided."
    exit 1
fi

if [ "${2}" == "" ]; then
    echo "Authentication-replated directory path must be provided."
    exit 1
fi

REPO="${1}"
AUTH="${2}"

cat <<EOF
version: "3.3"
services:
  docker_opt:
    image: docker:dind
    container_name: docker
    privileged: true
    environment:
    - DOCKER_TLS_CERTDIR=/certs
    entrypoint:
    - sh
    - -c
    - |
      mkdir -p /etc/docker/certs.d/${REGISTRY_HOST}:5000 && \
      cp /registry/certs/domain.crt /etc/docker/certs.d/${REGISTRY_HOST}:5000 && \
      dockerd-entrypoint.sh
    volumes:
    - docker-client:/certs/client
    - ${AUTH}:/registry:ro
  testenv_opt:
    build:
      context: "${REPO}/script/optimize/optimize"
      dockerfile: Dockerfile
    container_name: testenv_opt
    privileged: true
    working_dir: ${REPO_PATH}
    entrypoint: ./script/optimize/optimize/entrypoint.sh
    environment:
    - NO_PROXY=127.0.0.1,localhost,${REGISTRY_HOST}:5000
    - DOCKER_HOST=tcp://docker:2376
    - DOCKER_TLS_VERIFY=1
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:${REPO_PATH}:ro"
    - ${AUTH}:/auth:ro
    - docker-client:/docker/client:ro
    - /dev/fuse:/dev/fuse
  registry:
    image: registry:2
    container_name: ${REGISTRY_HOST}
    environment:
    - REGISTRY_AUTH=htpasswd
    - REGISTRY_AUTH_HTPASSWD_REALM="Registry Realm"
    - REGISTRY_AUTH_HTPASSWD_PATH=/auth/auth/htpasswd
    - REGISTRY_HTTP_TLS_CERTIFICATE=/auth/certs/domain.crt
    - REGISTRY_HTTP_TLS_KEY=/auth/certs/domain.key
    volumes:
    - ${AUTH}:/auth:ro
volumes:
  docker-client:
EOF
