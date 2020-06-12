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

REMOTE_SNAPSHOT_LABEL="containerd.io/snapshot/remote"
TEST_POD_NAME=testpod-$(head /dev/urandom | tr -dc a-z0-9 | head -c 10)
TEST_POD_NS=ns1
TEST_CONTAINER_NAME=testcontainer-$(head /dev/urandom | tr -dc a-z0-9 | head -c 10)

KIND_NODENAME="${1}"
KIND_KUBECONFIG="${2}"
TESTIMAGE="${3}"

echo "Creating testing pod...."
cat <<EOF | KUBECONFIG="${KIND_KUBECONFIG}" kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${TEST_POD_NAME}
  namespace: ${TEST_POD_NS}
spec:
  containers:
  - name: ${TEST_CONTAINER_NAME}
    image: ${TESTIMAGE}
    command: ["sleep"]
    args: ["infinity"]
  imagePullSecrets:
  - name: testsecret
EOF

echo "Checking created pod..."
IDX=0
DEADLINE=120
for (( ; ; )) ; do
    STATUS=$(KUBECONFIG="${KIND_KUBECONFIG}" kubectl get pods "${TEST_POD_NAME}" --namespace="${TEST_POD_NS}" -o 'jsonpath={..status.containerStatuses[0].state.running.startedAt}${..status.containerStatuses[0].state.waiting.reason}')
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

echo "Getting topmost layer from ${KIND_NODENAME}..."
TARGET_CONTAINER=
for (( RETRY=1; RETRY<=50; RETRY++ )) ; do
    echo "[${RETRY}]Trying to get container id..."
    TARGET_CONTAINER=$(docker exec -i "${KIND_NODENAME}" ctr-remote --namespace="k8s.io" c ls -q labels."io.kubernetes.container.name"=="${TEST_CONTAINER_NAME}" | sed -n 1p)
    if [ "${TARGET_CONTAINER}" != "" ] ; then
        break
    fi
    sleep 3
done
if [ "${TARGET_CONTAINER}" == "" ] ; then
    echo "no container created for ${TESTIMAGE}"
    docker exec -i "${KIND_NODENAME}" ctr-remote --namespace="k8s.io" c ls
    exit 1
else
    echo "container ${TARGET_CONTAINER} created for ${TESTIMAGE}"
fi
LAYER=$(docker exec -i "${KIND_NODENAME}" ctr-remote --namespace="k8s.io" \
               c info "${TARGET_CONTAINER}" \
            | jq -r '.SnapshotKey') # We don't check this *active* snapshot

echo "Checking all layers being remote snapshots..."
LAYERSNUM=0
for (( ; ; )) ; do
    LAYER=$(docker exec -i "${KIND_NODENAME}" ctr-remote --namespace="k8s.io" \
                   snapshot --snapshotter=stargz info "${LAYER}" \
                | jq -r '.Parent')
    if [ "${LAYER}" == "null" ] ; then
        break
    elif [ ${LAYERSNUM} -gt 100 ] ; then
        echo "testing image contains too many layes > 100"
        exit 1
    fi
    ((LAYERSNUM+=1))
    LABEL=$(docker exec -i "${KIND_NODENAME}" ctr-remote --namespace="k8s.io" \
                   snapshots --snapshotter=stargz info "${LAYER}" \
                | jq -r ".Labels.\"${REMOTE_SNAPSHOT_LABEL}\"")
    echo "Checking layer ${LAYER} : ${LABEL}"
    if [ "${LABEL}" == "null" ] ; then
        echo "layer ${LAYER} isn't remote snapshot"
        exit 1
    fi
done

if [ ${LAYERSNUM} -eq 0 ] ; then
    echo "cannot get layers"
    exit 1
fi

exit 0
