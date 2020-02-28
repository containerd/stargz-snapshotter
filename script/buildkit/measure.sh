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

RUN_SCRIPT=./script/buildkit/run.sh
STARGZ_SNAPSHOTTER_ADDRESS=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
BENCHMARK_LOG=/dev/null
if [ "${WITH_LOGFILE:-}" == "true" ] ; then
    BENCHMARK_LOG=$(mktemp)
    echo "log file: ${BENCHMARK_LOG}"
fi

function build_with_original {
    TARGET_CONTEXT="./script/buildkit/sample_org"
    echo "Context for buildctl: ${TARGET_CONTEXT}"
    time "${BUILDKIT_MASTER}/buildctl" build --progress=plain \
         --frontend=dockerfile.v0 \
         --local context="${TARGET_CONTEXT}" \
         --local dockerfile="${TARGET_CONTEXT}" $@ >> "${BENCHMARK_LOG}" 2>&1
}

function build_with_stargz {
    TARGET_CONTEXT="./script/buildkit/sample_sgz"
    echo "Context for buildctl: ${TARGET_CONTEXT}"
    time "${BUILDKIT_STARGZ}/buildctl" build --progress=plain \
         --frontend=dockerfile.v0 \
         --local context="${TARGET_CONTEXT}" \
         --local dockerfile="${TARGET_CONTEXT}" $@ >> "${BENCHMARK_LOG}" 2>&1
}

function run_with_original {
    echo "Arguments for buildkitd: $@"
    BUILDKIT_BIN_PATH="${BUILDKIT_MASTER}" "${RUN_SCRIPT}" $@ \
                     >> "${BENCHMARK_LOG}" 2>&1
}

function run_with_stargz {
    echo "Arguments for buildkitd: $@"
    BUILDKIT_BIN_PATH="${BUILDKIT_STARGZ}" "${RUN_SCRIPT}" $@ \
                     >> "${BENCHMARK_LOG}" 2>&1
}

TMPOCI_ORG=$(mktemp)
TMPOCI_SGZ=$(mktemp)
TMPCTD_ORG=$(mktemp)
TMPCTD_SGZ=$(mktemp)
RMLIST=( "${TMPOCI_ORG}" "${TMPOCI_SGZ}" "${TMPCTD_ORG}" "${TMPCTD_SGZ}" )

# measuring

run_with_original --oci-worker-snapshotter=overlayfs > "${TMPOCI_ORG}" 2>&1
build_with_original $@ >> "${TMPOCI_ORG}" 2>&1

run_with_stargz --oci-worker-snapshotter=stargz \
                --oci-worker-proxy-snapshotter-address="${STARGZ_SNAPSHOTTER_ADDRESS}" \
                > "${TMPOCI_SGZ}" 2>&1
build_with_stargz $@ >> "${TMPOCI_SGZ}" 2>&1

run_with_original --oci-worker=false --containerd-worker=true > "${TMPCTD_ORG}" 2>&1
build_with_original $@ >> "${TMPCTD_ORG}" 2>&1

run_with_stargz --oci-worker=false --containerd-worker=true \
                --containerd-worker-snapshotter=stargz \
                > "${TMPCTD_SGZ}" 2>&1
build_with_stargz $@ >> "${TMPCTD_SGZ}" 2>&1

# output

echo "## OCI Worker"

echo "### With original baseimage" | tee -a "${BENCHMARK_LOG}"
echo '```'
cat "${TMPOCI_ORG}"
echo '```'

echo "### With stargz baseimage" | tee -a "${BENCHMARK_LOG}"
echo '```'
cat "${TMPOCI_SGZ}"
echo '```'

echo "## containerd Worker"

echo "### With original baseimage" | tee -a "${BENCHMARK_LOG}"
echo '```'
cat "${TMPCTD_ORG}"
echo '```'

echo "### With stargz baseimage" | tee -a "${BENCHMARK_LOG}"
echo '```'
cat "${TMPCTD_SGZ}"
echo '```'

for RMFILE in "${RMLIST[@]}" ; do
    rm ${RMFILE}
done
