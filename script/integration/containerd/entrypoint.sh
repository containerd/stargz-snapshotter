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

PLUGINS=(remote)
REGISTRY_HOST=registry_integration
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

function check {
    if [ ${?} = 0 ] ; then
        echo "Completed: ${1}"
    else
        echo "Failed: ${1}"
        exit 1
    fi
}

function isServedAsRemoteSnapshot {
    LOG_PATH="${1}"
    if [ "$(cat ${LOG_PATH})" == "" ] ; then
        echo "Log is empty. Something is wrong."
        return 1
    fi

    LAYER_LOG=$(cat "${LOG_PATH}" | grep "layer-sha256:")
    if [ "${LAYER_LOG}" != "" ] ; then
        echo "Some layer have been downloaded by containerd"
        return 1
    fi
    return 0
}

CONTAINERD_ROOT=/var/lib/containerd/
function reboot_containerd {
    ps aux | grep containerd | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {}
    rm -rf "${CONTAINERD_ROOT}"*
    containerd ${@} &
    retry ctr version
}

function cleanup {
    ORG_EXIT_CODE="${1}"
    echo "Cleaning up /var/lib/rsnapshotd..."
    ls -1d /var/lib/rsnapshotd/io.containerd.snapshotter.v1.remote/snapshots/* | xargs -I{} echo "{}/fs" | xargs -I{} umount {}
    rm -rf /var/lib/rsnapshotd/*
    echo "Exit with code: ${ORG_EXIT_CODE}"
    exit "${ORG_EXIT_CODE}"
}

trap 'cleanup $?' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

# Log into the registry
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
check "Importing cert"

update-ca-certificates
check "Installing cert"

retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"
check "Login to the registry"

# Prepare images
gcrane cp ubuntu:18.04 "${REGISTRY_HOST}:5000/ubuntu:18.04"
stargzify "${REGISTRY_HOST}:5000/ubuntu:18.04" "${REGISTRY_HOST}:5000/ubuntu:stargz"
check "Stargzifying images"

# Wait for booting remote snapshotter
RETRYNUM=600 retry ls /run/rsnapshotd/rsnapshotd.sock
mkdir -p /etc/containerd && \
    cp ./script/integration/containerd/config.toml /etc/containerd/config.toml

############
# Tests for remote snapshotter
reboot_containerd --log-level debug --config=/etc/containerd/config.toml
NOTFOUND=false
for PLUGIN in ${PLUGINS[@]}; do
    OK=$(ctr plugins ls \
             | grep io.containerd.snapshotter \
             | sed -E 's/ +/ /g' \
             | cut -d ' ' -f 2,4 \
             | grep "${PLUGIN}" \
             | cut -d ' ' -f 2)
    if [ "${OK}" != "ok" ] ; then
        echo "Plugin ${PLUGIN} not found" 1>&2
        NOTFOUND=true
    fi
done

if [ "${NOTFOUND}" != "false" ] ; then
    exit 1
fi

############
# Tests for stargz filesystem
reboot_containerd --log-level debug --config=/etc/containerd/config.toml
ctr images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:18.04"
check "Getting normal image with normal snapshotter"
ctr run --rm "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr > /usr_normal_unstargz.tar

reboot_containerd --log-level debug --config=/etc/containerd/config.toml
ctr images pull --user "${DUMMYUSER}:${DUMMYPASS}" --skip-download --snapshotter=remote "${REGISTRY_HOST}:5000/ubuntu:18.04"
check "Getting normal image with remote snapshotter"
ctr run --rm --snapshotter=remote "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr > /usr_remote_unstargz.tar

reboot_containerd --log-level debug --config=/etc/containerd/config.toml
ctr images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:stargz"
check "Getting stargz image with normal snapshotter"
ctr run --rm "${REGISTRY_HOST}:5000/ubuntu:stargz" test tar -c /usr > /usr_normal_stargz.tar

PULL_LOG=$(mktemp)
check "Preparing log file"
reboot_containerd --log-level debug --config=/etc/containerd/config.toml
ctr images pull --user "${DUMMYUSER}:${DUMMYPASS}" --skip-download --snapshotter=remote "${REGISTRY_HOST}:5000/ubuntu:stargz" | tee "${PULL_LOG}"
check "Getting stargz image with remote snapshotter"
if ! isServedAsRemoteSnapshot "${PULL_LOG}" ; then
    echo "Failed to serve all layers as remote snapshots: ${LAYER_LOG}"
    exit 1
fi
rm "${PULL_LOG}"
ctr run --rm --snapshotter=remote "${REGISTRY_HOST}:5000/ubuntu:stargz" test tar -c /usr > /usr_remote_stargz.tar

mkdir /usr_normal_unstargz /usr_remote_unstargz /usr_normal_stargz /usr_remote_stargz
tar -xf /usr_normal_unstargz.tar -C /usr_normal_unstargz
check "Preparation for usr_normal_unstargz"

tar -xf /usr_remote_unstargz.tar -C /usr_remote_unstargz
check "Preparation for usr_remote_unstargz"

tar -xf /usr_normal_stargz.tar -C /usr_normal_stargz
check "Preparation for usr_normal_stargz"

tar -xf /usr_remote_stargz.tar -C /usr_remote_stargz
check "Preparation for usr_remote_stargz"

diff --no-dereference -qr /usr_normal_unstargz/ /usr_remote_unstargz/
check "Diff bitween two root filesystems(normal vs remote snapshotter, normal rootfs)"

diff --no-dereference -qr /usr_normal_stargz/ /usr_remote_stargz/
check "Diff bitween two root filesystems(normal vs remote snapshotter, stargzified rootfs)"

exit 0
