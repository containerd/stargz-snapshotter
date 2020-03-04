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

if [ "${1}" == "" ]; then
    echo "Repository path must be provided."
    exit 1
fi

if [ "${2}" == "" ]; then
    echo "Container name must be provided."
    exit 1
fi

REPO="${1}"
CONTAINER_NAME="${2}"

cat <<EOF
version: "3"
services:
  hello-bench:
    build:
      context: "${REPO}/script/benchmark/hello-bench"
      dockerfile: Dockerfile
    container_name: ${CONTAINER_NAME}
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
