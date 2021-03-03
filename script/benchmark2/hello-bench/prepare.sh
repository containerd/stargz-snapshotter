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
MEASURING_SCRIPT="${REPO}/script/benchmark2/hello-bench/src/hello.py"
REBOOT_CONTAINERD_SCRIPT="${REPO}/script/benchmark/hello-bench/reboot_containerd.sh"
NERDCTL_VERSION="0.5.0"

if [ $# -lt 1 ] ; then
    echo "Specify benchmark target."
    echo "Ex) ${0} <YOUR_REPOSITORY_NAME> --all"
    echo "Ex) ${0} <YOUR_REPOSITORY_NAME> alpine busybox"
    exit 1
fi
TARGET_REPOSITORY="${1}"
TARGET_IMAGES=${@:2}

if ! which ctr-remote ; then
    echo "ctr-remote not found, installing..."
    mkdir -p /tmp/out
    PREFIX=/tmp/out/ make clean && \
        PREFIX=/tmp/out/ make ctr-remote && \
        install /tmp/out/ctr-remote /usr/local/bin
fi

if ! which nerdctl ; then
    wget -O /tmp/nerdctl.tar.gz "https://github.com/AkihiroSuda/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-amd64.tar.gz"
    tar zxvf /tmp/nerdctl.tar.gz -C /usr/local/bin/
fi

NO_STARGZ_SNAPSHOTTER="true" "${REBOOT_CONTAINERD_SCRIPT}"
"${MEASURING_SCRIPT}" --repository=${TARGET_REPOSITORY} --op=prepare ${TARGET_IMAGES}
