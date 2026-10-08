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

# Preparing creds for provided user and password for private registry(only for testing purpose)
# See also: https://docs.docker.com/registry/deploying/
function prepare_creds {
    local OUTPUT="${1}"
    local REGISTRY_HOST="${2}"
    local USER="${3}"
    local PASS="${4}"
    mkdir "${OUTPUT}/auth" "${OUTPUT}/certs"
    openssl req -subj "/C=JP/ST=Remote/L=Snapshotter/O=TestEnv/OU=Integration/CN=${REGISTRY_HOST}" \
            -addext "subjectAltName = DNS:${REGISTRY_HOST}" \
            -newkey rsa:2048 -nodes -keyout "${OUTPUT}/certs/domain.key" \
            -x509 -days 365 -out "${OUTPUT}/certs/domain.crt"
    htpasswd -Bbn "${USER}" "${PASS}" > "${OUTPUT}/auth/htpasswd"
}

# Check if all snapshots logged in the specified file are prepared as remote snapshots.
# Whether a snapshot is prepared as a remote snapshot must be logged with the key
# "remote-snapshot-prepared" in JSON-formatted log.
# See also /snapshot/snapshot.go in this repo.
LOG_REMOTE_SNAPSHOT="remote-snapshot-prepared"
function check_remote_snapshots {
    local LOG_FILE="${1}"
    local REMOTE=0
    local LOCAL=0

    REMOTE=$(jq -r 'select(."'"${LOG_REMOTE_SNAPSHOT}"'" == "true")' "${LOG_FILE}" | wc -l)
    LOCAL=$(jq -r 'select(."'"${LOG_REMOTE_SNAPSHOT}"'" == "false")' "${LOG_FILE}" | wc -l)
    if [[ ${LOCAL} -gt 0 ]] ; then
        echo "some local snapshots creation have been reported (local:${LOCAL},remote:${REMOTE})"
        return 1
    elif [[ ${REMOTE} -gt 0 ]] ; then
        echo "all layers have been reported as remote snapshots (local:${LOCAL},remote:${REMOTE})"
        return 0
    else
        echo "no log for checking remote snapshot was provided; Is the log-level = debug?"
        return 1
    fi
}

# Get version from ARG directive in the specified Dockerfile.
function get_version_from_arg {
    local DOCKERFILE="${1}"
    local ARGNAME="${2}"
    cat "${DOCKERFILE}" | grep "${ARGNAME}=" | head -1 | sed -E 's/ARG +'"${ARGNAME}"'=v?([^ ]+).*/\1/g' | tr -d '\n'
}

# Get version of golang base image from the specified Dockerfile.
function go_base_version {
    local DOCKERFILE="${1}"
    cat "${DOCKERFILE}" | grep -E 'FROM\s+golang:' | head -1 | sed -E 's/FROM +[^:]*:([^ ]+).*/\1/g' | tr -d '\n'
}

function test_image_verification {
    KUBECONFIG_IN="${1}"
    PREPARE_NODE_NAME="${2}"
    REGISTRY="${3}"
    TEST_POD_NS="${4}"

    docker exec "${PREPARE_NODE_NAME}" /bin/sh -c '/out/ctr-remote images pull ghcr.io/stargz-containers/alpine:3.15.3-esgz'
    docker exec "${PREPARE_NODE_NAME}" /bin/sh -c '/out/ctr-remote images tag ghcr.io/stargz-containers/alpine:3.15.3-esgz '"${REGISTRY}"'/alpine:esgz'
    docker exec "${PREPARE_NODE_NAME}" /bin/sh -c '( cd /go/src/github.com/containerd/stargz-snapshotter/script/util/create-invalid-image/ ; go run main.go -estargz -- '"${REGISTRY}"'/alpine:esgz '"${REGISTRY}"'/alpine:esgz-invalid )'
    docker exec "${PREPARE_NODE_NAME}" /bin/sh -c '/out/ctr-remote images push -u "${REGISTRY_CREDS}" '"${REGISTRY}"'/alpine:esgz'
    docker exec "${PREPARE_NODE_NAME}" /bin/sh -c '/out/ctr-remote images push -u "${REGISTRY_CREDS}" '"${REGISTRY}"'/alpine:esgz-invalid'


    cat <<EOF | KUBECONFIG="${KUBECONFIG_IN}" kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: test1
  namespace: ${TEST_POD_NS}
spec:
  containers:
  - name: test1
    image: ${REGISTRY}/alpine:esgz-invalid
    command: ["sh"]
    args: ["-c", "sleep infinity"]
  imagePullSecrets:
  - name: testsecret
EOF

    echo "Waiting for pod creation (first pod)"
    IDX=0
    DEADLINE=120
    for (( ; ; )) ; do
        STATUS=$(KUBECONFIG="${KUBECONFIG_IN}" kubectl get pods test1 --namespace="${TEST_POD_NS}" -o 'jsonpath={..status.containerStatuses[0].state.running.startedAt}${..status.containerStatuses[0].state.waiting.reason}')
        echo "Status: ${STATUS}"
        STARTEDAT=$(echo "${STATUS}" | cut -f 1 -d '$')
        if [ "${STARTEDAT}" != "" ] ; then
            echo "Pod created"
            break
        elif [ ${IDX} -gt ${DEADLINE} ] ; then
            echo "Deadline exeeded to wait for pod creation"
            exit 1
        fi
        ((IDX+=1))
        sleep 1
    done

    cat <<EOF | KUBECONFIG="${KUBECONFIG_IN}" kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: test2
  namespace: ${TEST_POD_NS}
spec:
  containers:
  - name: test2
    image: ${REGISTRY}/alpine:esgz
    command: ["sh"]
    args: ["-c", "cat /modified.txt ; echo '' ; echo DONE ; sleep infinity"]
  imagePullSecrets:
  - name: testsecret
EOF

    echo "Waiting for pod creation (second pod)"
    IDX=0
    DEADLINE=120
    for (( ; ; )) ; do
        STATUS=$(KUBECONFIG="${KUBECONFIG_IN}" kubectl get pods test2 --namespace="${TEST_POD_NS}" -o 'jsonpath={..status.containerStatuses[0].state.running.startedAt}${..status.containerStatuses[0].state.waiting.reason}')
        echo "Status: ${STATUS}"
        STARTEDAT=$(echo "${STATUS}" | cut -f 1 -d '$')
        if [ "${STARTEDAT}" != "" ] ;  then
            echo "==========="
            KUBECONFIG="${KUBECONFIG_IN}" kubectl -n ${TEST_POD_NS} logs pod/test2
            echo "==========="
            DONESTRING=$(KUBECONFIG="${KUBECONFIG_IN}" kubectl -n ${TEST_POD_NS} logs pod/test2 | grep DONE)
            if [ "${DONESTRING}" = "DONE" ] ; then
                echo "Pod created"
                break
            fi
        elif [ ${IDX} -gt ${DEADLINE} ] ; then
            echo "Deadline exeeded to wait for pod creation"
            exit 1
        fi
        ((IDX+=1))
        sleep 1
    done

    if KUBECONFIG="${KUBECONFIG_IN}" kubectl -n ${TEST_POD_NS} logs pod/test2 | grep MODIFIED ; then
        echo "invalid contents detected"
        return 1
    fi

    return 0
}
