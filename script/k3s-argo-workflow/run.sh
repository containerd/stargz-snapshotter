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

K3S_VERSION=master
K3S_REPO=https://github.com/k3s-io/k3s

K3S_NODE_REPO=ghcr.io/stargz-containers
K3S_NODE_IMAGE_NAME=k3s
K3S_NODE_TAG=1
K3S_NODE_IMAGE="${K3S_NODE_REPO}/${K3S_NODE_IMAGE_NAME}:${K3S_NODE_TAG}"
K3S_CLUSTER_NAME="k3s-demo-cluster-$(date +%s%N | shasum | base64 | fold -w 10 | head -1)"

ORG_ARGOYAML=$(mktemp)
TMP_K3S_REPO=$(mktemp -d)
TMP_GOLANGCI=$(mktemp)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm "${ORG_ARGOYAML}" || true
    rm -rf "${TMP_K3S_REPO}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

function argo_yaml() {
    local IMAGE_TYPE="${1}"

    local TMP_CUSTOM_ARGOYAML=$(mktemp)
    cp "${ORG_ARGOYAML}" "${TMP_CUSTOM_ARGOYAML}"
    sed -i 's|containerRuntimeExecutor: docker|containerRuntimeExecutor: pns|g' "${TMP_CUSTOM_ARGOYAML}"

    local ARGOEXEC_IMAGE=argoproj/argoexec:v3.0.3
    local ARGOEXEC_IMAGE_USE=ghcr.io/stargz-containers/argoproj-argoexec:v3.0.3-"${IMAGE_TYPE}"
    replace_image "${TMP_CUSTOM_ARGOYAML}" "${ARGOEXEC_IMAGE}" "${ARGOEXEC_IMAGE_USE}"

    local MINIO_IMAGE=minio/minio:RELEASE.2019-12-17T23-16-33Z
    local MINIO_IMAGE_USE=ghcr.io/stargz-containers/minio:RELEASE.2019-12-17T23-16-33Z-"${IMAGE_TYPE}"
    replace_image "${TMP_CUSTOM_ARGOYAML}" "${MINIO_IMAGE}" "${MINIO_IMAGE_USE}"

    local ARGOWORKFLOW_IMAGE=argoproj/workflow-controller:v3.0.3
    local ARGOWORKFLOW_IMAGE_USE=ghcr.io/stargz-containers/argoproj-workflow-controller:v3.0.3-"${IMAGE_TYPE}"
    replace_image "${TMP_CUSTOM_ARGOYAML}" "${ARGOWORKFLOW_IMAGE}" "${ARGOWORKFLOW_IMAGE_USE}"

    local ARGOCLI_IMAGE=argoproj/argocli:v3.0.3
    local ARGOCLI_IMAGE_USE=ghcr.io/stargz-containers/argoproj-argocli:v3.0.3-"${IMAGE_TYPE}"
    replace_image "${TMP_CUSTOM_ARGOYAML}" "${ARGOCLI_IMAGE}" "${ARGOCLI_IMAGE_USE}"

    cat "${TMP_CUSTOM_ARGOYAML}"

    rm "${TMP_CUSTOM_ARGOYAML}"
}

function replace_image() {
    local TARGET_FILE="${1}"
    local IMAGE_ORG="${2}"
    local IMAGE_NEW="${3}"
    if ! cat "${TARGET_FILE}" | grep "${IMAGE_ORG}" > /dev/null 2>&1 ; then
        echo "error: image ${IMAGE_ORG} not specified" 1>&2
        cat "${TARGET_FILE}"
        exit 1
    fi
    sed -i "s|${IMAGE_ORG}|${IMAGE_NEW}|g" "${TARGET_FILE}"
}

function go_ci_yaml() {
    IMAGE_TYPE="${1}" envsubst < "${CONTEXT}/go.yaml.template"
}

function run {
    local IMAGE_TYPE="${1}"
    local SNAPSHOTTER="${2}"
    local ELAPSED_RESULT_FILE="${3}"

    # Prepare cluster configuration
    local CUSTOM_ARGOYAML=$(mktemp)
    argo_yaml "${IMAGE_TYPE}" > "${CUSTOM_ARGOYAML}"
    local TMP_GOCI_YAML=$(mktemp)
    go_ci_yaml "${IMAGE_TYPE}" > "${TMP_GOCI_YAML}"

    # Create argo cluster
    k3d cluster create "${K3S_CLUSTER_NAME}" --image="${K3S_NODE_IMAGE}" \
        --k3s-arg='--snapshotter='"${SNAPSHOTTER}"'@server:*;agent:*'
    kubectl create ns argo
    kubectl apply -n argo -f "${CUSTOM_ARGOYAML}"

    # Wait for the cluster is ready
    local RETRYNUM=30
    local RETRYINTERVAL=1
    local TIMEOUTSEC=180
    for i in $(seq ${RETRYNUM}) ; do
        if [ $(kubectl get -n argo pods -o json | jq -r '.items[]' | wc -l) -ne 0 ] ; then
            if [ $(kubectl get -n argo pods -o json | jq '.items[] | select(.status.phase != "Running" and .status.phase != "Succeeded")' | wc -l) -eq 0 ]
            then
                echo "argo is ready"
                break
            fi
        fi
        echo "Waiting for argo is ready..."
        sleep ${RETRYINTERVAL}
    done

    # Run the workflow and get the elapsed time
    argo submit -n argo --watch "${TMP_GOCI_YAML}"
    local START=$(argo list -n argo --completed -o json | jq -r '.[0].status.startedAt')
    local FINISH=$(argo list -n argo --completed -o json | jq -r '.[0].status.finishedAt')
    local ELAPSED=$(expr $(date --date "${FINISH}" +%s) - $(date --date "${START}" +%s))
    echo '{"type" : "'"${IMAGE_TYPE}"'", "snapshotter" : "'"${SNAPSHOTTER}"'", "elapsed" : "'"${ELAPSED}"'"}' | tee -a "${ELAPSED_RESULT_FILE}"

    # Finalize
    k3d cluster delete "${K3S_CLUSTER_NAME}"
    rm "${CUSTOM_ARGOYAML}"
    rm "${TMP_GOCI_YAML}"
}

RESULT_FILE="${RESULT:-}"
if [ "${RESULT_FILE}" == "" ] ; then
    RESULT_FILE=$(mktemp)
fi
echo "result to ${RESULT_FILE}"

wget -O "${ORG_ARGOYAML}" https://raw.githubusercontent.com/argoproj/argo-workflows/stable/manifests/quick-start-minimal.yaml

git clone -b ${K3S_VERSION} --depth 1 "${K3S_REPO}" "${TMP_K3S_REPO}"
( cd "${TMP_K3S_REPO}" && make generate )
cat <<EOF >> "${TMP_K3S_REPO}/go.mod"
replace github.com/containerd/stargz-snapshotter => "$(realpath ${REPO})"
replace github.com/containerd/stargz-snapshotter/estargz => "$(realpath ${REPO}/estargz)"
EOF
sed -i -E 's|(ENV DAPPER_RUN_ARGS .*)|\1 -v '"$(realpath ${REPO})":"$(realpath ${REPO})"':ro|g' "${TMP_K3S_REPO}/Dockerfile.dapper"
sed -i -E 's|(ENV DAPPER_ENV .*)|\1 DOCKER_BUILDKIT|g' "${TMP_K3S_REPO}/Dockerfile.dapper"
(
    cd "${TMP_K3S_REPO}" && \
        git config user.email "dummy@example.com" && \
        git config user.name "dummy" && \
        cat ./.golangci.json | jq '.run.deadline|="10m"' > "${TMP_GOLANGCI}" && \
        cp "${TMP_GOLANGCI}" ./.golangci.json &&  \
        make deps && \
        git add . && \
        git commit -m tmp && \
        REPO="${K3S_NODE_REPO}" IMAGE_NAME="${K3S_NODE_IMAGE_NAME}" TAG="${K3S_NODE_TAG}" make
)

#1
run "org" "overlayfs" "${RESULT_FILE}"
run "esgz" "stargz" "${RESULT_FILE}"

#2
run "org" "overlayfs" "${RESULT_FILE}"
run "esgz" "stargz" "${RESULT_FILE}"

#3
run "org" "overlayfs" "${RESULT_FILE}"
run "esgz" "stargz" "${RESULT_FILE}"
