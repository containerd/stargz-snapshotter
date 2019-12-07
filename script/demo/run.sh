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
( cd $GOPATH/src/github.com/ktock/remote-snapshotter && make -j2 && make install -j2 )
check "Preparing plugins"

echo "running containerd..."
containerd --config="${CONFIG}" $@ &
