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

PLUGIN=stargz
REGISTRY_HOST=registry-integration
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

RETRYNUM=100
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    SUCCESS=false
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            SUCCESS=true
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ "${SUCCESS}" == "true" ] ; then
        return 0
    else
        return 1
    fi
}

function isServedAsRemoteSnapshot {
    LOG_PATH="${1}"
    if [ "$(cat ${LOG_PATH})" == "" ] ; then
        echo "Log is empty. Something is wrong."
        return 1
    fi

    RES=$(cat "${LOG_PATH}" | grep -e 'application/vnd.oci.image.layer.\|application/vnd.docker.image.rootfs.')
    if [ "${RES}" != "" ] ; then
        echo "Some layer have been downloaded by containerd"
        return 1
    fi
    return 0
}

CONTAINERD_ROOT=/var/lib/containerd/
function reboot_containerd {
    ps aux | grep containerd | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {} || true
    rm -rf "${CONTAINERD_ROOT}"*
    containerd ${@} &
    retry ctr version
}

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"

echo "Preparing images..."
gcrane cp ubuntu:18.04 "${REGISTRY_HOST}:5000/ubuntu:18.04"
GO111MODULE=off PREFIX=/tmp/ctr/ make clean && \
    GO111MODULE=off PREFIX=/tmp/ctr/ make ctr-remote && \
    install /tmp/ctr/ctr-remote /usr/local/bin && \
    ctr-remote image optimize --stargz-only "${REGISTRY_HOST}:5000/ubuntu:18.04" "${REGISTRY_HOST}:5000/ubuntu:stargz"

echo "Waiting for booting stargz snapshotter..."
RETRYNUM=600 retry ls /run/containerd-stargz-grpc/containerd-stargz-grpc.sock
mkdir -p /etc/containerd && \
    cp ./script/integration/containerd/config.toml /etc/containerd/config.toml

############
# Tests for stargz snapshotter
reboot_containerd --log-level debug --config=/etc/containerd/config.toml
NOTFOUND=false
OK=$(ctr-remote plugins ls \
         | grep io.containerd.snapshotter \
         | sed -E 's/ +/ /g' \
         | cut -d ' ' -f 2,4 \
         | grep "${PLUGIN}" \
         | cut -d ' ' -f 2)
if [ "${OK}" != "ok" ] ; then
    echo "Plugin ${PLUGIN} not found" 1>&2
    exit 1
fi

############
# Tests for stargz filesystem
reboot_containerd --log-level debug --config=/etc/containerd/config.toml
echo "Getting normal image with normal snapshotter..."
ctr-remote images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:18.04"
ctr-remote run --rm "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr > /usr_normal_unstargz.tar

reboot_containerd --log-level debug --config=/etc/containerd/config.toml
echo "Getting normal image with stargz snapshotter..."
ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:18.04"
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr > /usr_remote_unstargz.tar

reboot_containerd --log-level debug --config=/etc/containerd/config.toml
echo "Getting stargz image with normal snapshotter..."
ctr-remote images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:stargz"
ctr-remote run --rm "${REGISTRY_HOST}:5000/ubuntu:stargz" test tar -c /usr > /usr_normal_stargz.tar

PULL_LOG=$(mktemp)
reboot_containerd --log-level debug --config=/etc/containerd/config.toml
echo "Getting stargz image with stargz snapshotter..."
ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:stargz" | tee "${PULL_LOG}"
if ! isServedAsRemoteSnapshot "${PULL_LOG}" ; then
    echo "Failed to serve all layers as remote snapshots"
    exit 1
fi
rm "${PULL_LOG}"
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/ubuntu:stargz" test tar -c /usr > /usr_remote_stargz.tar

echo "Extracting sample files..."
mkdir /usr_normal_unstargz /usr_remote_unstargz /usr_normal_stargz /usr_remote_stargz
tar -xf /usr_normal_unstargz.tar -C /usr_normal_unstargz
tar -xf /usr_remote_unstargz.tar -C /usr_remote_unstargz
tar -xf /usr_normal_stargz.tar -C /usr_normal_stargz
tar -xf /usr_remote_stargz.tar -C /usr_remote_stargz

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, normal rootfs)"
diff --no-dereference -qr /usr_normal_unstargz/ /usr_remote_unstargz/

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, stargzified rootfs)"
diff --no-dereference -qr /usr_normal_stargz/ /usr_remote_stargz/

exit 0
