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

set -eux -o pipefail

# NOTE: The entire contents of containerd/stargz-snapshotter are located in
# the testing container so utils.sh is visible from this script during runtime.
# TODO: Refactor the code dependencies and pack them in the container without
#       expecting and relying on volumes.
source "/utils.sh"

PLUGIN=stargz
REGISTRY_HOST=registry-integration.test
REGISTRY_ALT_HOST=registry-alt.test
DUMMYUSER=dummyuser
DUMMYPASS=dummypass

FUSE_MANAGER_LOG="Start snapshotter with fusemanager mode"

USR_ORG=$(mktemp -d)
USR_MIRROR=$(mktemp -d)
USR_REFRESH=$(mktemp -d)
USR_TMPDIR=$(mktemp -d)

USR_NORMALSN_UNSTARGZ=$(mktemp -d)
USR_STARGZSN_UNSTARGZ=$(mktemp -d)
USR_NORMALSN_STARGZ=$(mktemp -d)
USR_STARGZSN_STARGZ=$(mktemp -d)
USR_NORMALSN_ZSTD=$(mktemp -d)
USR_STARGZSN_ZSTD=$(mktemp -d)
USR_NORMALSN_PLAIN_STARGZ=$(mktemp -d)
USR_STARGZSN_PLAIN_STARGZ=$(mktemp -d)

USR_NORMALSN_IPFS=$(mktemp -d)
USR_STARGZSN_IPFS=$(mktemp -d)
USR_STARGZSN_CTD_IPFS=$(mktemp -d)

LOG_FILE=$(mktemp)
function cleanup {
    ORG_EXIT_CODE="${1}"
    rm -rf "${USR_ORG}" || true
    rm -rf "${USR_MIRROR}" || true
    rm -rf "${USR_REFRESH}" || true
    rm -rf "${USR_TMPDIR}" || true

    rm -rf "${USR_NORMALSN_UNSTARGZ}" || true
    rm -rf "${USR_STARGZSN_UNSTARGZ}" || true
    rm -rf "${USR_NORMALSN_STARGZ}" || true
    rm -rf "${USR_STARGZSN_STARGZ}" || true
    rm -rf "${USR_NORMALSN_PLAIN_STARGZ}" || true
    rm -rf "${USR_STARGZSN_PLAIN_STARGZ}" || true
    rm -rf "${USR_NORMALSN_ZSTD}" || true
    rm -rf "${USR_STARGZSN_ZSTD}" || true

    rm -rf "${USR_NORMALSN_IPFS}" || true
    rm -rf "${USR_STARGZSN_IPFS}" || true
    rm -rf "${USR_STARGZSN_CTD_IPFS}" || true
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
        TARGET_SIGNAL="-s ${2:-SIGKILL}"
        ps aux | grep "${1} " | grep -v grep | grep -v $(basename ${0}) | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill ${TARGET_SIGNAL} {} || true
    fi
}

function wait_all {
    while ps aux | grep -v grep | grep -v $(basename ${0}) | grep "${1} " ; do sleep 3 ; done
}

CONTAINERD_ROOT=/var/lib/containerd/
CONTAINERD_STATUS=/run/containerd/
REMOTE_SNAPSHOTTER_SOCKET=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
REMOTE_SNAPSHOTTER_ROOT=/var/lib/containerd-stargz-grpc/
REMOTE_SNAPSHOTTER_FUSE_MANAGER_SOCKET=/run/containerd-stargz-grpc/fuse-manager.sock
function reboot_containerd {
    kill_all "containerd"
    kill_all "containerd-stargz-grpc"
    kill_all "stargz-fuse-manager" SIGTERM
    rm -rf "${CONTAINERD_STATUS}"*
    rm -rf "${CONTAINERD_ROOT}"*
    if [ -e "${REMOTE_SNAPSHOTTER_SOCKET}" ] ; then
        rm "${REMOTE_SNAPSHOTTER_SOCKET}"
    fi
    if [ -e "${REMOTE_SNAPSHOTTER_FUSE_MANAGER_SOCKET}" ] ; then
        rm "${REMOTE_SNAPSHOTTER_FUSE_MANAGER_SOCKET}"
    fi
    if [ -d "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" ] ; then 
        find "${REMOTE_SNAPSHOTTER_ROOT}snapshotter/snapshots/" \
             -maxdepth 1 -mindepth 1 -type d -exec umount "{}/fs" \;
    fi
    rm -rf "${REMOTE_SNAPSHOTTER_ROOT}"*
    if [ "${BUILTIN_SNAPSHOTTER}" == "true" ] ; then
        if [ "${CONTAINERD_CONFIG:-}" != "" ] ; then
            containerd --log-level debug --config="${CONTAINERD_CONFIG:-}" 2>&1 | tee -a "${LOG_FILE}" &
        else
            containerd --log-level debug --config=/etc/containerd/config.toml 2>&1 | tee -a "${LOG_FILE}" &
        fi
    else
        if [ "${SNAPSHOTTER_CONFIG:-}" == "" ] ; then
            containerd-stargz-grpc --log-level=debug \
                                   --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
                                   2>&1 | tee -a "${LOG_FILE}" & # Dump all log
        else
            containerd-stargz-grpc --log-level=debug \
                                   --address="${REMOTE_SNAPSHOTTER_SOCKET}" \
                                   --config="${SNAPSHOTTER_CONFIG}" \
                                   2>&1 | tee -a "${LOG_FILE}" &
        fi
        retry ls "${REMOTE_SNAPSHOTTER_SOCKET}"
        containerd --log-level debug --config=/etc/containerd/config.toml &
        if [ "${NO_FUSE_MANAGER_CHECK:-}" != "true" ] ; then
            if cat "${LOG_FILE}" | grep "${FUSE_MANAGER_LOG}" ; then
                if [ "${FUSE_MANAGER}" != "true" ] ; then
                    echo "fuse manager should not be enabled"
                    exit 1
                fi
            else
                if [ "${FUSE_MANAGER}" == "true" ] ; then
                    echo "fuse manager should be enabled"
                    exit 1
                fi
            fi
        fi
    fi

    # Makes sure containerd and containerd-stargz-grpc are up-and-running.
    UNIQUE_SUFFIX=$(date +%s%N | shasum | base64 | fold -w 10 | head -1)
    retry ctr snapshots --snapshotter="${PLUGIN}" prepare "connectiontest-dummy-${UNIQUE_SUFFIX}" ""
}

function optimize {
    local SRC="${1}"
    local DST="${2}"
    local PLAINHTTP="${3}"
    local OPTS=${@:4}
    ctr-remote image pull -u "${DUMMYUSER}:${DUMMYPASS}" "${SRC}"
    ctr-remote image optimize ${OPTS} --oci "${SRC}" "${DST}"
    PUSHOPTS=
    if [ "${PLAINHTTP}" == "true" ] ; then
        PUSHOPTS=--plain-http
    fi
    ctr-remote image push ${PUSHOPTS} -u "${DUMMYUSER}:${DUMMYPASS}" "${DST}"
}

function convert {
    local SRC="${1}"
    local DST="${2}"
    local PLAINHTTP="${3}"
    local OPTS=${@:4}
    PUSHOPTS=
    if [ "${PLAINHTTP}" == "true" ] ; then
        PUSHOPTS=--plain-http
    fi
    ctr-remote image pull -u "${DUMMYUSER}:${DUMMYPASS}" "${SRC}"
    ctr-remote image convert ${OPTS} --oci "${SRC}" "${DST}"
    ctr-remote image push ${PUSHOPTS} -u "${DUMMYUSER}:${DUMMYPASS}" "${DST}"
}

function copy {
    local SRC="${1}"
    local DST="${2}"
    ctr-remote image pull --all-platforms "${SRC}"
    ctr-remote image tag "${SRC}" "${DST}"
    ctr-remote image push -u "${DUMMYUSER}:${DUMMYPASS}" "${DST}"
}

function copy_out_dir {
    local IMAGE="${1}"
    local TARGETDIR="${2}"
    local DEST="${3}"
    local SNAPSHOTTER="${4}"

    TMPFILE=$(mktemp)

    UNIQUE=$(date +%s%N | shasum | base64 | fold -w 10 | head -1)
    ctr-remote run --snapshotter="${SNAPSHOTTER}" "${IMAGE}" "${UNIQUE}" tar -c "${TARGETDIR}" > "${TMPFILE}"
    tar -C "${DEST}" -xf "${TMPFILE}"
    ctr-remote c rm "${UNIQUE}" || true
    rm "${TMPFILE}"
}

RPULL_COMMAND="rpull"
if [ "${USE_TRANSFER_SERVICE}" == "true" ] ; then
    RPULL_COMMAND="pull --snapshotter=stargz"
fi

function dump_dir {
    local IMAGE="${1}"
    local TARGETDIR="${2}"
    local SNAPSHOTTER="${3}"
    local REMOTE="${4}"
    local DEST="${5}"

    reboot_containerd
    if [ "${REMOTE}" == "true" ] ; then
        run_and_check_remote_snapshots ctr-remote images ${RPULL_COMMAND} --user "${DUMMYUSER}:${DUMMYPASS}" "${IMAGE}"
    else
        ctr-remote image pull --snapshotter="${SNAPSHOTTER}" --user "${DUMMYUSER}:${DUMMYPASS}" "${IMAGE}"
    fi
    copy_out_dir "${IMAGE}" "${TARGETDIR}" "${DEST}" "${SNAPSHOTTER}"
}

function run_and_check_remote_snapshots {
    echo -n "" > "${LOG_FILE}"
    ${@:1}
    check_remote_snapshots "${LOG_FILE}"
}

# Copied from moby project (Apache License 2.0)
# https://github.com/moby/moby/blob/a9fe88e395acaacd84067b5fc701d52dbcf4b625/hack/dind#L28-L38
# cgroup v2: enable nesting
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
	# move the processes from the root group to the /init group,
	# otherwise writing subtree_control fails with EBUSY.
	# An error during moving non-existent process (i.e., "cat") is ignored.
	mkdir -p /sys/fs/cgroup/init
	xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || :
	# enable controllers
	sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
		> /sys/fs/cgroup/cgroup.subtree_control
fi

echo "===== VERSION INFORMATION ====="
containerd --version
runc --version
echo "==============================="

cat <<EOF >> /etc/containerd/config.toml
[debug]
format = "json"
level = "debug"
EOF
if [ "${BUILTIN_SNAPSHOTTER}" != "true" ] ; then
    cat <<EOF >> /etc/containerd/config.toml
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
  [proxy_plugins.stargz.exports]
    root = "/var/lib/containerd-stargz-grpc/"
    enable_remote_snapshot_annotations = "true"
EOF
fi

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry nerdctl login -u "${DUMMYUSER}" -p "${DUMMYPASS}" "${REGISTRY_HOST}"

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

echo "Preparing images..."
copy ghcr.io/stargz-containers/ubuntu:22.04-org "${REGISTRY_HOST}/ubuntu:22.04"
copy ghcr.io/stargz-containers/alpine:3.15.3-org "${REGISTRY_HOST}/alpine:3.15.3"
stargzify "${REGISTRY_HOST}/ubuntu:22.04" "${REGISTRY_HOST}/ubuntu:sgz"
optimize "${REGISTRY_HOST}/ubuntu:22.04" "${REGISTRY_HOST}/ubuntu:esgz" "false"
optimize "${REGISTRY_HOST}/ubuntu:22.04" "${REGISTRY_HOST}/ubuntu:zstdchunked" "false" --zstdchunked
optimize "${REGISTRY_HOST}/alpine:3.15.3" "${REGISTRY_HOST}/alpine:esgz" "false"
optimize "${REGISTRY_HOST}/alpine:3.15.3" "${REGISTRY_ALT_HOST}:5000/alpine:esgz" "true"

# images for external TOC and min-chunk-size
# TODO: support external TOC suffix other than "-esgztoc"
optimize "${REGISTRY_HOST}/ubuntu:22.04" "${REGISTRY_HOST}/ubuntu:esgz-50000" "false" --estargz-min-chunk-size=50000
optimize "${REGISTRY_HOST}/ubuntu:22.04" "${REGISTRY_HOST}/ubuntu:esgz-ex" "false" --estargz-external-toc
ctr-remote image push -u "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/ubuntu:esgz-ex-esgztoc"
convert "${REGISTRY_HOST}/ubuntu:22.04" "${REGISTRY_HOST}/ubuntu:esgz-ex-keep-diff-id" "false" --estargz --estargz-external-toc --estargz-keep-diff-id
ctr-remote image push -u "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/ubuntu:esgz-ex-keep-diff-id-esgztoc"

if [ "${BUILTIN_SNAPSHOTTER}" != "true" ] ; then

    ############
    # Tests with IPFS if standalone snapshotter
    echo "Testing with IPFS..."

    export IPFS_PATH="/tmp/ipfs"
    mkdir -p "${IPFS_PATH}"

    ipfs init
    ipfs daemon --offline &
    retry curl -X POST localhost:5001/api/v0/version >/dev/null 2>&1 # wait for up

    # stargz snapshotter (default labels)
    ctr-remote image pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/ubuntu:22.04"
    CID=$(ctr-remote i ipfs-push "${REGISTRY_HOST}/ubuntu:22.04")
    reboot_containerd
    run_and_check_remote_snapshots ctr-remote i rpull --ipfs "${CID}"
    copy_out_dir "${CID}" "/usr" "${USR_STARGZSN_IPFS}" "stargz"

    # stargz snapshotter (containerd labels)
    ctr-remote image pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/ubuntu:22.04"
    CID=$(ctr-remote i ipfs-push "${REGISTRY_HOST}/ubuntu:22.04")
    reboot_containerd
    run_and_check_remote_snapshots ctr-remote i rpull --use-containerd-labels --ipfs "${CID}"
    copy_out_dir "${CID}" "/usr" "${USR_STARGZSN_CTD_IPFS}" "stargz"

    # overlayfs snapshotter
    ctr-remote image pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/ubuntu:22.04"
    CID=$(ctr-remote i ipfs-push --estargz=false "${REGISTRY_HOST}/ubuntu:22.04")
    reboot_containerd
    ctr-remote i rpull --snapshotter=overlayfs --ipfs "${CID}"
    copy_out_dir "${CID}" "/usr" "${USR_NORMALSN_IPFS}" "overlayfs"

    echo "Diffing between two root filesystems(normal vs stargz snapshotter(default labels), IPFS rootfs)"
    diff --no-dereference -qr "${USR_NORMALSN_IPFS}/" "${USR_STARGZSN_IPFS}/"

    echo "Diffing between two root filesystems(normal vs stargz snapshotter(containerd labels), IPFS rootfs)"
    diff --no-dereference -qr "${USR_NORMALSN_IPFS}/" "${USR_STARGZSN_CTD_IPFS}/"

    kill_all ipfs
fi

############
# Tests for refreshing and mirror
echo "Testing refreshing and mirror..."

reboot_containerd
echo "Getting image with normal snapshotter..."
ctr-remote image pull --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/alpine:esgz"
copy_out_dir "${REGISTRY_HOST}/alpine:esgz" "/usr" "${USR_ORG}" "overlayfs"

echo "Getting image with stargz snapshotter..."
run_and_check_remote_snapshots ctr-remote images ${RPULL_COMMAND} --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/alpine:esgz"

REGISTRY_HOST_IP=$(getent hosts "${REGISTRY_HOST}" | awk '{ print $1 }')
REGISTRY_ALT_HOST_IP=$(getent hosts "${REGISTRY_ALT_HOST}" | awk '{ print $1 }')

echo "Disabling source registry and check if mirroring is working for stargz snapshotter..."
iptables -A OUTPUT -d "${REGISTRY_HOST_IP}" -j DROP
iptables -L
copy_out_dir "${REGISTRY_HOST}/alpine:esgz" "/usr" "${USR_MIRROR}" "stargz"
iptables -D OUTPUT -d "${REGISTRY_HOST_IP}" -j DROP

echo "Disabling mirror registry and check if refreshing works for stargz snapshotter..."
iptables -A OUTPUT -d "${REGISTRY_ALT_HOST_IP}" -j DROP
iptables -L
copy_out_dir "${REGISTRY_HOST}/alpine:esgz" "/usr" "${USR_REFRESH}" "stargz"
iptables -D OUTPUT -d "${REGISTRY_ALT_HOST_IP}" -j DROP

echo "Disabling all registries and running container should fail"
iptables -A OUTPUT -d "${REGISTRY_HOST_IP}","${REGISTRY_ALT_HOST_IP}" -j DROP
iptables -L
if ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}/alpine:esgz" test tar -c /usr > /usr_dummy_fail.tar ; then
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

# Test with a normal image

echo "Getting normal image with normal snapshotter..."
OVERLAYFSSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:22.04" "/usr" "overlayfs" "false" "${OVERLAYFSSN}"

echo "Getting normal image with stargz snapshotter..."
STARGZSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:22.04" "/usr" "stargz" "false" "${STARGZSN}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, normal rootfs)"
diff --no-dereference -qr "${OVERLAYFSSN}/" "${STARGZSN}/"

# Test with an eStargz image

echo "Getting eStargz image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:esgz" "/usr" "overlayfs" "false" "${USR_NORMALSN_STARGZ}"

echo "Getting eStargz image with stargz snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:esgz" "/usr" "stargz" "true" "${USR_STARGZSN_STARGZ}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, eStargz rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_STARGZ}/" "${USR_STARGZSN_STARGZ}/"

# Test with a zstd:chunked image

echo "Getting zstd:chunked image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:zstdchunked" "/usr" "overlayfs" "false" "${USR_NORMALSN_ZSTD}"

echo "Getting zstd:chunked image with stargz snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:zstdchunked" "/usr" "stargz" "true" "${USR_STARGZSN_ZSTD}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, zstd:cunked rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_ZSTD}/" "${USR_STARGZSN_ZSTD}/"

# Test with a external TOC image

echo "Getting external TOC image with normal snapshotter..."
EX_OVERLAYFSSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:esgz-ex" "/usr" "overlayfs" "false" "${EX_OVERLAYFSSN}"

echo "Getting external TOC image with stargz snapshotter..."
EX_STARGZSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:esgz-ex" "/usr" "stargz" "true" "${EX_STARGZSN}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, eStargz (external TOC) rootfs)"
diff --no-dereference -qr "${EX_OVERLAYFSSN}/" "${EX_STARGZSN}/"

# Test with a external TOC + keep-diff-id image

echo "Getting external TOC image (keep diff id) with normal snapshotter..."
EXLL_OVERLAYFSSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:esgz-ex-keep-diff-id" "/usr" "overlayfs" "false" "${EXLL_OVERLAYFSSN}"

echo "Getting external TOC image (keep diff id) with stargz snapshotter..."
EXLL_STARGZSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:esgz-ex-keep-diff-id" "/usr" "stargz" "true" "${EXLL_STARGZSN}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, eStargz (external TOC, keep-diff-id) rootfs)"
diff --no-dereference -qr "${EXLL_OVERLAYFSSN}/" "${EXLL_STARGZSN}/"

# Test with a eStargz with min-chunk-size=50000 image

echo "Getting eStargz (min-chunk-size=50000) with normal snapshotter..."
E50000_OVERLAYFSSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:esgz-50000" "/usr" "overlayfs" "false" "${E50000_OVERLAYFSSN}"

echo "Getting eStargz (min-chunk-size=50000) with stargz snapshotter..."
E50000_STARGZSN=$(mktemp -d --tmpdir="${USR_TMPDIR}")
dump_dir "${REGISTRY_HOST}/ubuntu:esgz-50000" "/usr" "stargz" "true" "${E50000_STARGZSN}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, eStargz (min-chunk-size=50000) rootfs)"
diff --no-dereference -qr "${E50000_OVERLAYFSSN}/" "${E50000_STARGZSN}/"

############
# Checking compatibility with plain stargz

reboot_containerd
echo "Getting (legacy) stargz image with normal snapshotter..."
dump_dir "${REGISTRY_HOST}/ubuntu:sgz" "/usr" "overlayfs" "false" "${USR_NORMALSN_PLAIN_STARGZ}"

echo "Getting (legacy) stargz image with stargz snapshotter..."
TEST_CONTAINERD_CONFIG=
TEST_SNAPSHOTTER_CONFIG=
if [ "${BUILTIN_SNAPSHOTTER}" == "true" ] ; then
    cp /etc/containerd/config.toml /tmp/config.containerd.noverify.toml
    sed -i 's/disable_verification = false/disable_verification = true/g' /tmp/config.containerd.noverify.toml
    TEST_CONTAINERD_CONFIG="/tmp/config.containerd.noverify.toml"
else
    echo "disable_verification = true" > /tmp/config.stargz.noverify.toml
    cat /etc/containerd-stargz-grpc/config.toml >> /tmp/config.stargz.noverify.toml
    TEST_SNAPSHOTTER_CONFIG="/tmp/config.stargz.noverify.toml"
fi

CONTAINERD_CONFIG="${TEST_CONTAINERD_CONFIG}" SNAPSHOTTER_CONFIG="${TEST_SNAPSHOTTER_CONFIG}" \
                 dump_dir "${REGISTRY_HOST}/ubuntu:sgz" "/usr" "stargz" "true" "${USR_STARGZSN_PLAIN_STARGZ}"

echo "Diffing between two root filesystems(normal vs stargz snapshotter, plain stargz rootfs)"
diff --no-dereference -qr "${USR_NORMALSN_PLAIN_STARGZ}/" "${USR_STARGZSN_PLAIN_STARGZ}/"

############
# Try to pull this image from different namespace.
ctr-remote --namespace=dummy images ${RPULL_COMMAND} --user "${DUMMYUSER}:${DUMMYPASS}" \
           "${REGISTRY_HOST}/ubuntu:esgz"

############
# Test for starting when no configuration file.
mv /etc/containerd-stargz-grpc/config.toml /etc/containerd-stargz-grpc/config.toml_rm
NO_FUSE_MANAGER_CHECK=true reboot_containerd
mv /etc/containerd-stargz-grpc/config.toml_rm /etc/containerd-stargz-grpc/config.toml

############
# Test graceful restart

function check_cache_empty {
    TARGET_DIRS=("${REMOTE_SNAPSHOTTER_ROOT}stargz/httpcache/" "${REMOTE_SNAPSHOTTER_ROOT}stargz/fscache/")
    for D in "${TARGET_DIRS[@]}" ; do
        test -z "$( ls -A $D )"
    done
}

reboot_containerd
if [ "${BUILTIN_SNAPSHOTTER}" != "true" ] ; then
    # Snapshots should be available even after restarting the snapshotter with a signal
    run_and_check_remote_snapshots ctr-remote images ${RPULL_COMMAND} --user "${DUMMYUSER}:${DUMMYPASS}" "${REGISTRY_HOST}/alpine:esgz"
    ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}/alpine:esgz" test echo hi

    TARGET_SIGNALS=(SIGINT SIGTERM)
    for S in "${TARGET_SIGNALS[@]}" ; do
        # Kill the snapshotter
        kill_all containerd-stargz-grpc "$S"

        # wait until stargz snapshotter is finished
        wait_all containerd-stargz-grpc

        if [ "$S" == "SIGINT" ] ; then
            # On SIGINT, fuse manager mode also performs graceful shutdown so test this behaviour.
            if [ "${FUSE_MANAGER}" == "true" ] ; then
                kill_all stargz-fuse-manager "$S"
                wait_all stargz-fuse-manager
            fi
            # Check if resource are cleaned up
            check_cache_empty
        else
            if [ "${FUSE_MANAGER}" != "true" ] ; then
                # If this is not FUSE manager mode, check if resource are cleaned up
                check_cache_empty
            fi
        fi

        # Restart the snapshotter without additional operation
        containerd-stargz-grpc --log-level=debug --address="${REMOTE_SNAPSHOTTER_SOCKET}" 2>&1 | tee -a "${LOG_FILE}" & # Dump all log
        retry nc -z -U "${REMOTE_SNAPSHOTTER_SOCKET}"
        sleep 3 # FIXME: Additional wait; sometimes snapshotter is still unavailable after the socket ready

        # Check if the snapshotter is still usable
        ctr-remote run --rm --snapshotter=stargz "${REGISTRY_HOST}/alpine:esgz" test echo hi
    done
fi

exit 0
