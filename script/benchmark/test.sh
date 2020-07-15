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

BENCHMARKING_BASE_IMAGE_NAME="benchmark-image-base"
BENCHMARKING_NODE_IMAGE_NAME="benchmark-image-test"
BENCHMARKING_NODE=hello-bench
BENCHMARKING_CONTAINER=hello-bench-container

if [ "${BENCHMARKING_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."
    docker build -t "${BENCHMARKING_BASE_IMAGE_NAME}" --target snapshotter-base \
           ${DOCKER_BUILD_ARGS:-} "${REPO}"
fi

DOCKER_COMPOSE_YAML=$(mktemp)
TMP_CONTEXT=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm "${DOCKER_COMPOSE_YAML}" || true
    rm -rf "${TMP_CONTEXT}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

cp -R "${CONTEXT}/config" "${TMP_CONTEXT}"

cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${BENCHMARKING_BASE_IMAGE_NAME}

RUN apt-get update -y && \
    apt-get --no-install-recommends install -y python jq && \
    git clone https://github.com/google/go-containerregistry \
              \${GOPATH}/src/github.com/google/go-containerregistry && \
    cd \${GOPATH}/src/github.com/google/go-containerregistry && \
    git checkout 4b1985e5ea2104672636879e1694808f735fd214 && \
    GO111MODULE=on go install github.com/google/go-containerregistry/cmd/crane

COPY ./config/config.containerd.toml /etc/containerd/config.toml
COPY ./config/config.stargz.toml /etc/containerd-stargz-grpc/config.toml

ENV CONTAINERD_SNAPSHOTTER=""

ENTRYPOINT [ "sleep", "infinity" ]
EOF
docker build -t "${BENCHMARKING_NODE_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

echo "Preparing docker-compose.yml..."
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3"
services:
  ${BENCHMARKING_NODE}:
    image: ${BENCHMARKING_NODE_IMAGE_NAME}
    container_name: ${BENCHMARKING_CONTAINER}
    privileged: true
    working_dir: /go/src/github.com/containerd/stargz-snapshotter
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

echo "Benchmaring..."
BENCHMARKING_LOG=$(mktemp)
echo "log file: ${BENCHMARKING_LOG}"
FAIL=
if ! ( cd "${CONTEXT}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} \
                          "${BENCHMARKING_NODE}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
           docker exec -e BENCHMARK_SAMPLES_NUM -i "${BENCHMARKING_CONTAINER}" script/benchmark/hello-bench/run.sh \
                  "${BENCHMARK_USER}" ${BENCHMARK_TARGETS} \
               | tee "${BENCHMARKING_LOG}" ) ; then
    FAIL=true
else
    LOGDIR=$(mktemp -d)
    cat "${BENCHMARKING_LOG}" | "${CONTEXT}/tools/format.sh" | "${CONTEXT}/tools/plot.sh" "${LOGDIR}"
    cat "${BENCHMARKING_LOG}" | "${CONTEXT}/tools/format.sh" | "${CONTEXT}/tools/table.sh" > "${LOGDIR}/result.md"
    mv "${BENCHMARKING_LOG}" "${LOGDIR}/result.log"
    echo "See logs for >>> ${LOGDIR}"
    OUTPUTDIR="${BENCHMARK_RESULT_DIR:-}"
    if [ "${OUTPUTDIR}" != "" ] ; then
        cp "${LOGDIR}/result.md" "${LOGDIR}/result.png" "${LOGDIR}/result.log" "${OUTPUTDIR}"
        cat "${LOGDIR}/result.log" | "${CONTEXT}/tools/format.sh" > "${OUTPUTDIR}/result.json"
    fi
fi

echo "Cleaning up environment..."
docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0
