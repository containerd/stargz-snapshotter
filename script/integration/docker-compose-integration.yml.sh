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

REGISTRY_HOST=registry-integration

if [ "${1}" == "" ]; then
    echo "Repository path must be provided."
    exit 1
fi

if [ "${2}" == "" ]; then
    echo "Authentication-replated directory path must be provided."
    exit 1
fi

if [ "${3}" == "" ]; then
    echo "Temp dir for /var/lib/containerd-stargz-grpc must be provided."
    exit 1
fi

REPO="${1}"
AUTH="${2}"
SS_ROOT_DIR="${3}"

cat <<EOF
version: "3"
services:
  testenv_integration:
    build:
      context: "${REPO}/script/integration/containerd"
      dockerfile: Dockerfile
    container_name: testenv_integration
    privileged: true
    working_dir: /go/src/github.com/ktock/stargz-snapshotter
    entrypoint: ./script/integration/containerd/entrypoint.sh
    environment:
    - GO111MODULE=off
    - NO_PROXY=127.0.0.1,localhost,${REGISTRY_HOST}:5000
    - HTTP_PROXY=${HTTP_PROXY:-}
    - HTTPS_PROXY=${HTTPS_PROXY:-}
    - http_proxy=${http_proxy:-}
    - https_proxy=${https_proxy:-}
    tmpfs:
    - /var/lib/containerd
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:/go/src/github.com/ktock/stargz-snapshotter:ro"
    - ${AUTH}:/auth
    - "${SS_ROOT_DIR}:/var/lib/containerd-stargz-grpc:rshared"
    - ssstate:/run/containerd-stargz-grpc
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
    - ${AUTH}:/auth
  remote_snapshotter_integration:
    build:
      context: "${REPO}/script/integration/containerd-stargz-grpc"
      dockerfile: Dockerfile
    container_name: remote_snapshotter_integration
    privileged: true
    working_dir: /go/src/github.com/ktock/stargz-snapshotter
    entrypoint: ./script/integration/containerd-stargz-grpc/entrypoint.sh
    environment:
    - GO111MODULE=off
    - NO_PROXY=127.0.0.1,localhost,${REGISTRY_HOST}:5000
    - HTTP_PROXY=${HTTP_PROXY:-}
    - HTTPS_PROXY=${HTTPS_PROXY:-}
    - http_proxy=${http_proxy:-}
    - https_proxy=${https_proxy:-}
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:/go/src/github.com/ktock/stargz-snapshotter:ro"
    - "${AUTH}:/auth"
    - "${SS_ROOT_DIR}:/var/lib/containerd-stargz-grpc:rshared"
    - ssstate:/run/containerd-stargz-grpc
    - /dev/fuse:/dev/fuse
volumes:
  ssstate:
EOF
