#!/bin/bash

CONFIG=/etc/containerd/config.toml
CONTAINERD_ROOT=/var/lib/containerd
PLUGINS_DIR=/opt/containerd/plugins
SNAPSHOT_NAME=remote

function check {
    if [ $? -ne 0 ] ; then
        (>&2 echo "Failed: ${1}")
        exit 1
    fi
}

function prepare_plugin {
    go build -buildmode=plugin -o "${PLUGINS_DIR}/remotesn-linux-amd64.so" "${GOPATH}/src/github.com/ktock/remote-snapshotter"
    check "prepare remote snapshotter plugin"

    go build -buildmode=plugin -o "${PLUGINS_DIR}/stargzfs-linux-amd64.so" "${GOPATH}/src/github.com/ktock/remote-snapshotter/filesystems/stargz"
    check "prepare stargz filesystem plugin"
}

function kill_all_containerd {
    ps aux | grep containerd | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {}
}

function cleanup_root {
    ls -1d "${CONTAINERD_ROOT}/io.containerd.snapshotter.v1.${SNAPSHOT_NAME}/snapshots/"* | xargs -I{} echo "{}/fs" | xargs -I{} umount {}
    rm -rf "${CONTAINERD_ROOT}/"*
}

echo "cleaning up the environment..."
kill_all_containerd
cleanup_root

echo "preparing plugins..."
prepare_plugin

echo "running containerd..."
containerd --config="${CONFIG}" &
