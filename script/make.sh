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
REGISTRY_HOST_KIND=kind-private-registry
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
PULLSECRETS=false
CRI_TEST=false
for T in ${@} ; do
    case "${T}" in
        "integration" ) INTEGRATION=true ;;
        "test-optimize" ) OPTIMIZE=true ;;
        "test-pullsecrets" ) PULLSECRETS=true ;;
        "test-cri" ) CRI_TEST=true ;;
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

if [ "${PULLSECRETS}" == "true" ] ; then
    AUTH_DIR=$(mktemp -d)
    DOCKERCONFIG=$(mktemp)
    prepare_creds "${AUTH_DIR}" "${REGISTRY_HOST_KIND}" "${DUMMYUSER}" "${DUMMYPASS}"
    echo -n '{"auths":{"'"${REGISTRY_HOST_KIND}"':5000":{"auth":"'$(echo -n "${DUMMYUSER}:${DUMMYPASS}" | base64 -i -w 0)'"}}}' > "${DOCKERCONFIG}"

    echo "Preparing private registry..."
    TESTIMAGE="${REGISTRY_HOST_KIND}:5000/library/ubuntu:18.04"
    REGISTRY_NETWORK_KIND=kind_registry_network
    DOCKER_COMPOSE_YAML=$(mktemp)
    CONTEXT="${REPO}/script/pullsecrets"
    cd "${CONTEXT}"
    "${CONTEXT}"/docker-compose-privateregistry.yml.sh "${REGISTRY_HOST_KIND}" \
                "${REGISTRY_NETWORK_KIND}" "${AUTH_DIR}" > "${DOCKER_COMPOSE_YAML}"
    if docker network create "${REGISTRY_NETWORK_KIND}" && \
            docker-compose -f "${DOCKER_COMPOSE_YAML}" up -d --force-recreate && \
            docker run --rm -i --network "${REGISTRY_NETWORK_KIND}" \
                   -v "${AUTH_DIR}/certs/domain.crt:/usr/local/share/ca-certificates/rgst.crt:ro" \
                   -v "${DOCKERCONFIG}:/root/.docker/config.json:ro" \
                   -v "${REPO}:/go/src/github.com/containerd/stargz-snapshotter:ro" \
                   golang:1.13-buster /bin/bash -c "update-ca-certificates && cd /go/src/github.com/containerd/stargz-snapshotter && PREFIX=/out/ make ctr-remote && /out/ctr-remote images optimize --stargz-only ubuntu:18.04 ${TESTIMAGE}" ; then
        echo "Completed to prepare private registry"
    else
        echo "Failed to prepare private registry"
        FAIL=true
    fi

    KIND_KUBECONFIG=$(mktemp)
    echo "Testing in kind cluster (kubeconfig: ${KIND_KUBECONFIG})..."
    KIND_CLUSTER_NAME=kind-stargz-snapshotter
    if [ "${FAIL}" != "true" ] && \
           "${CONTEXT}"/run-kind.sh "${KIND_CLUSTER_NAME}" \
                       "${KIND_KUBECONFIG}" \
                       "${AUTH_DIR}/certs/domain.crt" \
                       "${REPO}" \
                       "${REGISTRY_NETWORK_KIND}" \
                       "${DOCKERCONFIG}" && \
           echo "Waiting until secrets fullly synced..." && \
           sleep 30 && \
           echo "Trying to pull private image with secret..." && \
           "${CONTEXT}"/test.sh "$(kind get nodes --name "${KIND_CLUSTER_NAME}" | sed -n 1p)" \
                       "${KIND_KUBECONFIG}" "${TESTIMAGE}" ; then
        echo "Successfully created remote snapshotter with private registry"
    else
        echo "Failed to create remote snapshotter with private registry"
        FAIL=true
    fi

    echo "Cleaning up environment..."
    docker-compose -f "${DOCKER_COMPOSE_YAML}" down -v
    kind delete cluster --name "${KIND_CLUSTER_NAME}"
    docker network rm "${REGISTRY_NETWORK_KIND}"
    rm "${DOCKERCONFIG}"
    rm "${DOCKER_COMPOSE_YAML}"
    rm "${KIND_KUBECONFIG}"
    rm -rf "${AUTH_DIR}"
fi

if [ "${CRI_TEST}" == "true" ] ; then
    IMAGE_LIST=$(mktemp)
    if ! ( "${REPO}/script/cri/build.sh" "${REPO}" && \
               "${REPO}/script/cri/test-legacy.sh" "${IMAGE_LIST}" && \
               "${REPO}/script/cri/test-stargz.sh" "${IMAGE_LIST}" ) ; then
        FAIL=true
    fi
    rm "${IMAGE_LIST}"
fi

if [ "$TARGETS" != "" ] ; then
    MINI_CONTEXT=$(mktemp -d)
    cat <<EOF > "${MINI_CONTEXT}/Dockerfile"
FROM golang:1.13
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse
EOF
    IMAGE_NAME="minienv:$(sha256sum ${MINI_CONTEXT}/Dockerfile | cut -f 1 -d ' ')"
    if ! ( docker build "${MINI_CONTEXT}" -t "${IMAGE_NAME}" ${DOCKER_BUILD_ARGS:-} && \
               docker run --rm --privileged --device /dev/fuse \
                      --tmpfs /tmp:exec,mode=777 \
                      -w /go/src/github.com/containerd/stargz-snapshotter \
                      -v "${REPO}:/go/src/github.com/containerd/stargz-snapshotter" \
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
