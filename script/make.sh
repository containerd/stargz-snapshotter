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

REGISTRY_HOST_INTEGRATION=registry-integration
REGISTRY_HOST_OPTIMIZE=registry-optimize
DUMMYUSER=dummyuser
DUMMYPASS=dummypass
REPO="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )/../"

function prepare_creds {
    AUTH_DIR="${1}"
    REGISTRY_HOST="${2}"
    USER="${3}"
    PASS="${4}"
    # See also: https://docs.docker.com/registry/deploying/
    echo "Preparing creds..."
    mkdir "${AUTH_DIR}/auth" "${AUTH_DIR}/certs"
    openssl req -subj "/C=JP/ST=Remote/L=Snapshotter/O=TestEnv/OU=Integration/CN=${REGISTRY_HOST}" \
            -newkey rsa:2048 -nodes -keyout "${AUTH_DIR}/certs/domain.key" \
            -x509 -days 365 -out "${AUTH_DIR}/certs/domain.crt"
    docker run --entrypoint htpasswd registry:2 -Bbn "${USER}" "${PASS}" > "${AUTH_DIR}/auth/htpasswd"
}

if [ "${1}" == "" ]; then
    echo "No make command provided"
    exit 1
fi

# NOTE: Specify build args via ${DOCKER_BUILD_ARGS}
echo "Build arguments: ${DOCKER_BUILD_ARGS:-}"

TARGETS=
INTEGRATION=false
OPTIMIZE=false
BENCHMARK=false
for T in ${@} ; do
    case "${T}" in
        "integration" ) INTEGRATION=true ;;
        "test-optimize" ) OPTIMIZE=true ;;
        "benchmark" ) BENCHMARK=true ;;
        * ) TARGETS="${TARGETS} ${T}" ;;
    esac
done

FAIL=false
if [ "${INTEGRATION}" == "true" ] ; then
    AUTH_DIR=$(mktemp -d)
    prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST_INTEGRATION}" "${DUMMYUSER}" "${DUMMYPASS}"

    echo "Preparing docker-compose.yml..."
    DOCKER_COMPOSE_YAML=$(mktemp)
    RS_ROOT_DIR=$(mktemp -d)
    CONTEXT="${REPO}/script/integration"
    cd "${CONTEXT}"
    "${CONTEXT}"/docker-compose-integration.yml.sh "${REPO}" "${AUTH_DIR}" "${RS_ROOT_DIR}" > "${DOCKER_COMPOSE_YAML}"

    if ! ( docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} testenv_integration remote_snapshotter_integration && \
               docker-compose -f "${DOCKER_COMPOSE_YAML}" up --abort-on-container-exit ) ; then
        FAIL=true
    fi

    echo "Cleaning up environment..."
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
    rm "${DOCKER_COMPOSE_YAML}"
    rm -rf "${AUTH_DIR}"
    rm -rf "${RS_ROOT_DIR}"
fi

if [ "${BENCHMARK}" == "true" ] ; then
    echo "Preparing docker-compose.yml..."

    DOCKER_COMPOSE_YAML=$(mktemp)
    CONTEXT="${REPO}/script/benchmark"
    cd "${CONTEXT}"
    CONTAINER_NAME=hello-bench
    "${CONTEXT}"/docker-compose-benchmark.yml.sh "${REPO}" "${CONTAINER_NAME}" > "${DOCKER_COMPOSE_YAML}"

    BENCHMARK_LOG=$(mktemp)
    echo "log file: ${BENCHMARK_LOG}"
    if ! ( docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} hello-bench && \
               docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
               docker exec -i "${CONTAINER_NAME}" script/benchmark/hello-bench/run.sh "${BENCHMARK_USER}" ${BENCHMARK_TARGETS} \
                   | tee "${BENCHMARK_LOG}" ) ; then
        FAIL=true
    else
        LOGDIR=$(mktemp -d)
        cat "${BENCHMARK_LOG}" | "${CONTEXT}/tools/format.sh" | "${CONTEXT}/tools/plot.sh" "${LOGDIR}"
        cat "${BENCHMARK_LOG}" | "${CONTEXT}/tools/format.sh" | "${CONTEXT}/tools/table.sh" > "${LOGDIR}/result.md"
        mv "${BENCHMARK_LOG}" "${LOGDIR}/result.log"
        echo "See logs for >>> ${LOGDIR}"
        OUTPUTDIR="${BENCHMARK_RESULT_DIR:-}"
        if [ "${OUTPUTDIR}" != "" ] ; then
            cp "${LOGDIR}/result.md" "${LOGDIR}/result.png" "${LOGDIR}/result.log" "${OUTPUTDIR}"
            cat "${LOGDIR}/result.log" | "${CONTEXT}/tools/format.sh" > "${OUTPUTDIR}/result.json"
        fi
    fi

    echo "Cleaning up environment..."
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
    rm "${DOCKER_COMPOSE_YAML}"
fi

if [ "${OPTIMIZE}" == "true" ] ; then
    AUTH_DIR=$(mktemp -d)
    prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST_OPTIMIZE}" "${DUMMYUSER}" "${DUMMYPASS}"

    echo "Preparing docker-compose.yml..."
    DOCKER_COMPOSE_YAML=$(mktemp)
    CONTEXT="${REPO}/script/optimize"
    cd "${CONTEXT}"
    "${CONTEXT}"/docker-compose-opt.yml.sh "${REPO}" "${AUTH_DIR}" > "${DOCKER_COMPOSE_YAML}"

    if ! ( docker-compose -f "${DOCKER_COMPOSE_YAML}" build ${DOCKER_BUILD_ARGS:-} testenv_opt && \
               docker-compose -f "${DOCKER_COMPOSE_YAML}" up --abort-on-container-exit ) ; then
        FAIL=true
    fi

    echo "Cleaning up environment..."
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
    rm "${DOCKER_COMPOSE_YAML}"
    rm -rf "${AUTH_DIR}"
fi

if [ "$TARGETS" != "" ] ; then
    MINI_CONTEXT=$(mktemp -d)
    cat <<EOF > "${MINI_CONTEXT}/Dockerfile"
FROM golang:1.14
RUN apt-get update -y && apt-get install -y fuse
EOF
    IMAGE_NAME="minienv:$(sha256sum ${MINI_CONTEXT}/Dockerfile | cut -f 1 -d ' ')"
    if ! ( docker build "${MINI_CONTEXT}" -t "${IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} && \
               docker run --rm --privileged --device /dev/fuse \
                      --tmpfs /tmp:exec,mode=777 \
                      -w /go/src/github.com/containerd/stargz-snapshotter \
                      -v "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro" \
                      "${IMAGE_NAME}" make $TARGETS PREFIX=/tmp/out/ ) ; then
        FAIL=true
    fi

    echo "Cleaning up environment..."
    rm -r "${MINI_CONTEXT}"
fi

if [ "${FAIL}" == "true" ] ; then
    echo "Some targets failed."
    exit 1
fi

echo "Succeeded all."
exit 0
