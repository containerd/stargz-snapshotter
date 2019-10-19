#!/bin/bash

CONFIG=/etc/containerd/config.toml
CONTAINERD_ROOT=/var/lib/containerd
PLUGINS_DIR=/opt/containerd/plugins
SNAPSHOT_NAME=remote

function prepare_plugin {
    go build -buildmode=plugin -o "${PLUGINS_DIR}/remotesn-linux-amd64.so" "${GOPATH}/src/github.com/ktock/remote-snapshotter" || (echo "failed to prepare remote snapshotter plugin" && exit 1)
    go build -buildmode=plugin -o "${PLUGINS_DIR}/stargzfs-linux-amd64.so" "${GOPATH}/src/github.com/ktock/remote-snapshotter/filesystems/stargz" || (echo "failed to prepare stargz filesystem plugin" && exit 1)
}

function kill_all_containerd {
    ps aux | grep containerd | grep -v grep | sed -E 's/ +/ /g' | cut -f 2 -d ' ' | xargs -I{} kill -9 {}
}

function cleanup_root {
    ls -1d "${CONTAINERD_ROOT}/io.containerd.snapshotter.v1.${SNAPSHOT_NAME}/snapshots/*" | xargs -I{} echo "{}/fs" | xargs -I{} umount {}
    rm -rf "${CONTAINERD_ROOT}/*"
}

echo "cleaning up the environment..."
kill_all_containerd
cleanup_root

echo "preparing plugins..."
prepare_plugin

echo "running containerd..."
containerd --config="${CONFIG}" &
