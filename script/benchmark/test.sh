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
    docker build ${DOCKER_BUILD_ARGS:-} -t "${BENCHMARKING_BASE_IMAGE_NAME}" \
           --target snapshotter-base "${REPO}"
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
    GO111MODULE=on go get github.com/google/go-containerregistry/cmd/crane

COPY ./config/config.containerd.toml /etc/containerd/config.toml
COPY ./config/config.stargz.toml /etc/containerd-stargz-grpc/config.toml

ENV CONTAINERD_SNAPSHOTTER=""

ENTRYPOINT [ "sleep", "infinity" ]
EOF
docker build -t "${BENCHMARKING_NODE_IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} "${TMP_CONTEXT}"

echo "Preparing docker-compose.yml..."
cat <<EOF > "${DOCKER_COMPOSE_YAML}"
version: "3.7"
services:
  ${BENCHMARKING_NODE}:
    image: ${BENCHMARKING_NODE_IMAGE_NAME}
    container_name: ${BENCHMARKING_CONTAINER}
    privileged: true
    init: true
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
volumes:
  containerd-data:
  containerd-stargz-grpc-data:
EOF

echo "Preparing for benchmark..."
OUTPUTDIR="${BENCHMARK_RESULT_DIR:-}"
if [ "${OUTPUTDIR}" == "" ] ; then
    OUTPUTDIR=$(mktemp -d)
fi
echo "See output for >>> ${OUTPUTDIR}"
LOG_DIR="${BENCHMARK_LOG_DIR:-}"
if [ "${LOG_DIR}" == "" ] ; then
    LOG_DIR=$(mktemp -d)
fi
LOG_FILE="${LOG_DIR}/containerd-stargz-grpc-benchmark-$(date '+%Y%m%d%H%M%S')"
touch "${LOG_FILE}"
echo "Logging to >>> ${LOG_FILE} (will finally be stored under ${OUTPUTDIR})"

echo "Benchmarking..."
FAIL=
if ! ( cd "${CONTEXT}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} \
                          "${BENCHMARKING_NODE}" && \
           docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
           docker exec -e BENCHMARK_SAMPLES_NUM -i "${BENCHMARKING_CONTAINER}" \
                  script/benchmark/hello-bench/run.sh \
                  "${BENCHMARK_REGISTRY:-docker.io}/${BENCHMARK_USER}" \
                  ${BENCHMARK_TARGETS} &> "${LOG_FILE}" ) ; then
    echo "Failed to run benchmark."
    FAIL=true
fi

echo "Harvesting log ${LOG_FILE} -> ${OUTPUTDIR} ..."
tar zcvf "${OUTPUTDIR}/result.log.tar.gz" "${LOG_FILE}"
if [ "${FAIL}" != "true" ] ; then
    echo "Formatting output..."
    if ! ( tar zOxf "${OUTPUTDIR}/result.log.tar.gz" | "${CONTEXT}/tools/format.sh" > "${OUTPUTDIR}/result.json" && \
           cat "${OUTPUTDIR}/result.json" | "${CONTEXT}/tools/plot.sh" "${OUTPUTDIR}" && \
           cat "${OUTPUTDIR}/result.json" | "${CONTEXT}/tools/percentiles.sh" "${OUTPUTDIR}" && \
           cat "${OUTPUTDIR}/result.json" | "${CONTEXT}/tools/table.sh" > "${OUTPUTDIR}/result.md" && \
           cat "${OUTPUTDIR}/result.json" | "${CONTEXT}/tools/csv.sh" > "${OUTPUTDIR}/result.csv" ) ; then
        echo "Failed to formatting output (but you can try it manually from ${OUTPUTDIR})"
        FAIL=true
    fi
fi

echo "Cleaning up environment..."
docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0
