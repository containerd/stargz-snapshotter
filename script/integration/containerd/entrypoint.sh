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
REPO="${CONTEXT}../../../"

# NOTE: The entire contents of containerd/stargz-snapshotter are located in
# the testing container so utils.sh is visible from this script during runtime.
# TODO: Refactor the code dependencies and pack them in the container without
#       expecting and relying on volumes.
source "${REPO}/script/util/utils.sh"

PLUGIN=stargz
REGISTRY_HOST=registry-integration
REGISTRY_ALT_HOST=registry-alt
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

TMP_DIR=$(mktemp -d)
USR_ORG=$(mktemp -d)
USR_MIRROR=$(mktemp -d)
USR_REFRESH=$(mktemp -d)
USR_NOMALSN_UNSTARGZ=$(mktemp -d)
USR_NOMALSN_STARGZ=$(mktemp -d)
USR_STARGZSN_UNSTARGZ=$(mktemp -d)
USR_STARGZSN_STARGZ=$(mktemp -d)
LOG_FILE=$(mktemp)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm -rf "${TMP_DIR}" || true
    rm -rf "${TMP_DIR}" || true
    rm -rf "${USR_ORG}" || true
    rm -rf "${USR_MIRROR}" || true
    rm -rf "${USR_REFRESH}" || true
    rm -rf "${USR_NOMALSN_UNSTARGZ}" || true
    rm -rf "${USR_NOMALSN_STARGZ}" || true
    rm -rf "${USR_STARGZSN_UNSTARGZ}" || true
    rm -rf "${USR_STARGZSN_STARGZ}" || true
    rm "${LOG_FILE}"
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

RETRYNUM=100
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    local SUCCESS=false
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

function kill_all {
    if [ "${1}" != "" ] ; then
        ps aux | grep "${1}" | grep -v grep | grep -v $(basename ${0}) | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {} || true
    fi
}

CONTAINERD_ROOT=/var/lib/containerd/
REMOTE_SNAPSHOTTER_SOCKET=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
REMOTE_SNAPSHOTTER_ROOT=/var/lib/containerd-stargz-grpc/
function reboot_containerd {
    kill_all "containerd"
    kill_all "containerd-stargz-grpc"
    rm -rf "${CONTAINERD_ROOT}"*
    if [ -f "${REMOTE_SNAPSHOTTER_SOCKET}" ] ; then
        rm "${REMOTE_SNAPSHOTTER_SOCKET}"
    fi
    if [ -d "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" ] ; then 
        find "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" \
             -maxdepth 1 -mindepth 1 -type d -exec umount "{}/fs" \;
    fi
    rm -rf "${REMOTE_SNAPSHOTTER_ROOT}"*
    containerd-stargz-grpc --log-level=debug \
                           --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
                           --config=/etc/containerd-stargz-grpc/config.toml \
                           2>&1 | tee -a "${LOG_FILE}" & # Dump all log
    retry ls "${REMOTE_SNAPSHOTTER_SOCKET}"
    containerd --log-level debug --config=/etc/containerd/config.toml &
    retry ctr version
}

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"

echo "Installing snapshotter..."
PREFIX="${TMP_DIR}/" make clean
PREFIX="${TMP_DIR}/" GO_BUILD_FLAGS="-race" make containerd-stargz-grpc # Check data race
PREFIX="${TMP_DIR}/" make ctr-remote
PREFIX="${TMP_DIR}/" make install
mkdir -p /etc/containerd /etc/containerd-stargz-grpc
cp ./script/integration/containerd/config.containerd.toml /etc/containerd/config.toml
cp ./script/integration/containerd/config.stargz.toml /etc/containerd-stargz-grpc/config.toml

echo "Preparing images..."
crane copy ubuntu:18.04 "${REGISTRY_HOST}:5000/ubuntu:18.04"
crane copy alpine:3.10.2 "${REGISTRY_HOST}:5000/alpine:3.10.2"
ctr-remote image optimize --stargz-only "${REGISTRY_HOST}:5000/ubuntu:18.04" "${REGISTRY_HOST}:5000/ubuntu:stargz"
ctr-remote image optimize --stargz-only "${REGISTRY_HOST}:5000/alpine:3.10.2" "${REGISTRY_HOST}:5000/alpine:stargz"
ctr-remote image optimize --stargz-only --plain-http "${REGISTRY_HOST}:5000/alpine:3.10.2" "http://${REGISTRY_ALT_HOST}:5000/alpine:stargz"

############
# Tests for stargz snapshotter
reboot_containerd
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
# Tests for refreshing and mirror
echo "Testing refreshing and mirror..."

reboot_containerd
echo "Getting image with normal snapshotter..."
ctr-remote images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/alpine:stargz"
ctr-remote run --rm "${REGISTRY_HOST}:5000/alpine:stargz" test tar -c /usr | tar -xC "${USR_ORG}"

echo "Getting image with stargz snapshotter..."
echo -n "" > "${LOG_FILE}"
ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/alpine:stargz"
check_remote_snapshots "${LOG_FILE}"

REGISTRY_HOST_IP=$(getent hosts "${REGISTRY_HOST}" | awk '{ print $1 }')
REGISTRY_ALT_HOST_IP=$(getent hosts "${REGISTRY_ALT_HOST}" | awk '{ print $1 }')

echo "Disabling source registry and check if mirroring is working for stargz snapshotter..."
iptables -A OUTPUT -d "${REGISTRY_HOST_IP}" -j DROP
iptables -L
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/alpine:stargz" test tar -c /usr \
    | tar -xC "${USR_MIRROR}"
iptables -D OUTPUT -d "${REGISTRY_HOST_IP}" -j DROP

echo "Disabling mirror registry and check if refreshing works for stargz snapshotter..."
iptables -A OUTPUT -d "${REGISTRY_ALT_HOST_IP}" -j DROP
iptables -L
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/alpine:stargz" test tar -c /usr \
    | tar -xC "${USR_REFRESH}"
iptables -D OUTPUT -d "${REGISTRY_ALT_HOST_IP}" -j DROP

echo "Disabling all registries and running container should fail"
iptables -A OUTPUT -d "${REGISTRY_HOST_IP}","${REGISTRY_ALT_HOST_IP}" -j DROP
iptables -L
if ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/alpine:stargz" test tar -c /usr > /usr_dummy_fail.tar ; then
    echo "All registries are disabled so this must be failed"
    exit 1
else
    echo "Failed to run the container as expected"
fi
iptables -D OUTPUT -d "${REGISTRY_HOST_IP}","${REGISTRY_ALT_HOST_IP}" -j DROP

echo "Diffing root filesystems for mirroring"
diff --no-dereference -qr "${USR_ORG}/" "${USR_MIRROR}/"

echo "Diffing root filesystems for refreshing"
diff --no-dereference -qr "${USR_ORG}/" "${USR_REFRESH}/"

############
# Tests for stargz filesystem
echo "Testing stargz filesystem..."

reboot_containerd
echo "Getting normal image with normal snapshotter..."
ctr-remote images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:18.04"
ctr-remote run --rm "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr \
    | tar -xC "${USR_NOMALSN_UNSTARGZ}"

reboot_containerd
echo "Getting normal image with stargz snapshotter..."
ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:18.04"
ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/ubuntu:18.04" test tar -c /usr \
    | tar -xC "${USR_STARGZSN_UNSTARGZ}"

reboot_containerd
echo "Getting stargz image with normal snapshotter..."
ctr-remote images pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:stargz"
ctr-remote run --rm "${REGISTRY_HOST}:5000/ubuntu:stargz" test tar -c /usr \
    | tar -xC "${USR_NOMALSN_STARGZ}"

reboot_containerd
echo "Getting stargz image with stargz snapshotter..."
echo -n "" > "${LOG_FILE}"
ctr-remote images rpull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}:5000/ubuntu:stargz"
check_remote_snapshots "${LOG_FILE}"

ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}:5000/ubuntu:stargz" test tar -c /usr \
    | tar -xC "${USR_STARGZSN_STARGZ}"

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, normal rootfs)"
diff --no-dereference -qr "${USR_NOMALSN_UNSTARGZ}/" "${USR_STARGZSN_UNSTARGZ}/"

echo "Diffing bitween two root filesystems(normal vs stargz snapshotter, stargzified rootfs)"
diff --no-dereference -qr "${USR_NOMALSN_STARGZ}/" "${USR_STARGZSN_STARGZ}/"

exit 0
