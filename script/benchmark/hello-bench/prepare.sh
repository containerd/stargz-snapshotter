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

MEASURING_SCRIPT=./script/benchmark/hello-bench/src/hello.py

if [ $# -lt 1 ] ; then
    echo "Specify benchmark target."
    echo "Ex) ${0} <YOUR_ACCOUNT_NAME> --all"
    echo "Ex) ${0} <YOUR_ACCOUNT_NAME> alpine busybox"
    exit 1
fi
TARGET_REPOUSER="${1}"
TARGET_IMAGES=${@:2}

if ! which ctr-remote ; then
    echo "ctr-remote not found, installing..."
    mkdir -p /tmp/out
    GO111MODULE=off PREFIX=/tmp/out/ make clean && \
        GO111MODULE=off PREFIX=/tmp/out/ make ctr-remote && \
        install /tmp/out/ctr-remote /usr/local/bin
fi

"${MEASURING_SCRIPT}" --user=${TARGET_REPOUSER} --op=prepare ${TARGET_IMAGES}
