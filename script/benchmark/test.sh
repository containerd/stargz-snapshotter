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

# This project's Dockerfile doesn't work without BuildKit.
export DOCKER_BUILDKIT=1

BENCHMARK_TARGETS="${BENCHMARK_TARGETS:-}"
if [[ -z "$BENCHMARK_TARGETS" ]]; then
  echo "BENCHMARK_TARGETS must be specified."
  exit 1
fi

BENCHMARKING_BASE_IMAGE_NAME="benchmark-image-base"
BENCHMARKING_NODE_IMAGE_NAME="benchmark-image-test"
BENCHMARKING_NODE=hello-bench
BENCHMARKING_CONTAINER=hello-bench-container
BENCHMARK_USER=${BENCHMARK_USER:-stargz-containers}
export BENCHMARK_RUNTIME_MODE=${BENCHMARK_RUNTIME_MODE:-containerd}

BENCHMARKING_TARGET_BASE_IMAGE=
BENCHMARKING_TARGET_CONFIG_DIR=
if [ "${BENCHMARK_RUNTIME_MODE}" == "containerd" ] ; then
    BENCHMARKING_TARGET_BASE_IMAGE=snapshotter-base
    BENCHMARKING_TARGET_CONFIG_DIR="${CONTEXT}/config-containerd"
elif [ "${BENCHMARK_RUNTIME_MODE}" == "podman" ] ; then
    BENCHMARKING_TARGET_BASE_IMAGE=podman-base
    BENCHMARKING_TARGET_CONFIG_DIR="${CONTEXT}/config-podman"
else
    echo "Unknown runtime: ${BENCHMARK_RUNTIME_MODE}"
    exit 1
fi

if [ "${BENCHMARKING_NO_RECREATE:-}" != "true" ] ; then
    echo "Preparing node image..."
    docker build ${DOCKER_BUILD_ARGS:-} -t "${BENCHMARKING_BASE_IMAGE_NAME}" \
           --target "${BENCHMARKING_TARGET_BASE_IMAGE}" "${REPO}"
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

cp -R "${BENCHMARKING_TARGET_CONFIG_DIR}" "${TMP_CONTEXT}/config"

cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM ${BENCHMARKING_BASE_IMAGE_NAME}

RUN apt-get update -y && \
    apt-get install -y python3 jq wget && \
    ln -s /usr/bin/python3 /usr/bin/python && \
    mkdir -p /tmp/crane && \
    wget -O - https://github.com/google/go-containerregistry/releases/download/v0.19.1/go-containerregistry_Linux_x86_64.tar.gz | tar -C /tmp/crane/ -zxf - && \
    mv /tmp/crane/crane /usr/local/bin/

COPY ./config /

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
    - "containers-data:/var/lib/containers:delegated"
    - "additional-store-data:/var/lib/stargz-store:delegated"
volumes:
  containerd-data:
  containerd-stargz-grpc-data:
  containers-data:
  additional-store-data:
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
           docker compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} \
                          "${BENCHMARKING_NODE}" && \
           docker compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
           docker exec \
		  -e BENCHMARK_RUNTIME_MODE -e BENCHMARK_SAMPLES_NUM -e BENCHMARK_PROFILE \
                  -i "${BENCHMARKING_CONTAINER}" \
                  script/benchmark/hello-bench/run.sh \
                  "${BENCHMARK_REGISTRY:-ghcr.io}/${BENCHMARK_USER}" \
                  ${BENCHMARK_TARGETS} &> "${LOG_FILE}" ) ; then
    echo "Failed to run benchmark."
    FAIL=true
fi

echo "Collecting data inside ${BENCHMARKING_CONTAINER}..."
docker exec -i "${BENCHMARKING_CONTAINER}" \
  tar zcf - -C /tmp/hello-bench-output . | tar zxvf - -C "$OUTPUTDIR"

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
docker compose -f "${DOCKER_COMPOSE_YAML}" down -v
if [ "${FAIL}" == "true" ] ; then
    exit 1
fi

exit 0
