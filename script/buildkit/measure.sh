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

RUN_SCRIPT="./script/buildkit/run.sh"
SAMPLE_DIR="./script/buildkit/sample/"

STARGZ_SNAPSHOTTER_ADDRESS=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
DOCKERFILE_SUFFIX_PH='{{ IMAGE_MODE_SUFFIX }}'
IMAGE_DEST_DIR=/benchmark/image/
ORG_DEST_FILE="${IMAGE_DEST_DIR}/org.tar"
SGZ_DEST_FILE="${IMAGE_DEST_DIR}/sgz.tar"
ESGZ_DEST_FILE="${IMAGE_DEST_DIR}/esgz.tar"

TEST_ENABLE=true
if [ "${WITHOUT_TEST:-}" == "true" ] ; then
    TEST_ENABLE=false
fi

BENCHMARK_LOG="${WITH_LOGFILE:-}"
if [ "${BENCHMARK_LOG}" == "" ] ; then
    BENCHMARK_LOG=/dev/null
fi

BUILDKIT_WORKER="${WITH_WORKER:-}"
if [ "${BUILDKIT_WORKER}" == "" ] ; then
    BUILDKIT_WORKER="oci"
fi

MODES=( ${TARGET_MODES:-} )
if [ ${#MODES[@]} -eq 0 ] || [ "${TEST_ENABLE}" == "true"] ; then
    MODES=("legacy" "stargz" "estargz")
fi

IMAGES=( ${TARGET_IMAGES:-} )
if [ ${#IMAGES[@]} -eq 0 ] ; then
    IMAGES=( $(ls -1 "${SAMPLE_DIR}" | xargs echo) )
fi

function build {
    local BUILDCTL_BIN="${1}"
    local TARGET_CONTEXT="${2}"
    local TARGET_DOCKERFILE="${3}"
    local DEST_FILE="${4}"
    local RESULT=$(mktemp)
    /usr/bin/time --output="${RESULT}" -f '%e' "${BUILDCTL_BIN}/buildctl" build --progress=plain \
         --frontend=dockerfile.v0 \
         --local context="${TARGET_CONTEXT}" \
         --local dockerfile="${TARGET_DOCKERFILE}" \
         --output type=docker,name=sample,dest="${DEST_FILE}" \
         >> "${BENCHMARK_LOG}" 2>&1
    local ELAPSED=$(cat "${RESULT}")
    rm "${RESULT}"
    echo -n "${ELAPSED}"
}

function run {
    local BUILDKITD_BIN="${1}"
    BUILDKIT_BIN_PATH="${BUILDKITD_BIN}" "${RUN_SCRIPT}" ${@:2} \
                     >> "${BENCHMARK_LOG}" 2>&1
}

# is_same_image checks the contents of two images are the same
#
# It compares the following contents of each image are the same:
# - Config file
# - Layer blobs
#   - but the timestamp differences are ignored
#   - FIXME: Also compare the timestamp differences.
#            But in some cases, images created in the different timings
#            possibly contains files which have same paths but have
#            different time stamps
function is_same_image {
    local IMG1="${1}"
    local IMG2="${2}"
    echo "Comparing image ${IMG1} and ${IMG2}" >> "${BENCHMARK_LOG}"
    
    # compares config files
    local IMG1_CONFIG=$(tar xf "${IMG1}" manifest.json -O | jq -r '.[0].Config')
    local IMG2_CONFIG=$(tar xf "${IMG2}" manifest.json -O | jq -r '.[0].Config')
    local SUM_CFG1=$(tar xf "${IMG1}" "${IMG1_CONFIG}" -O \
          | jq '.rootfs = {} | .created = "" | .history = []' \
          | sha256sum | cut -d ' ' -f 1)
    local SUM_CFG2=$(tar xf "${IMG2}" "${IMG2_CONFIG}" -O \
          | jq '.rootfs = {} | .created = "" | .history = []' \
          | sha256sum | cut -d ' ' -f 1)
    echo "Sums of configs are ${SUM_CFG1} and ${SUM_CFG2}" >> "${BENCHMARK_LOG}"
    if ! [ "${SUM_CFG1}" == "${SUM_CFG2}" ] ; then
        return 1
    fi

    # compares layer blobs
    local IMG1_LAYERS=( $(tar xf "${IMG1}" manifest.json -O | jq -r '.[0].Layers[]') )
    local IMG2_LAYERS=( $(tar xf "${IMG2}" manifest.json -O | jq -r '.[0].Layers[]') )
    if ! [ ${#IMG1_LAYERS[@]} -eq ${#IMG2_LAYERS[@]} ] ; then
        return 1
    fi
    local I=
    for I in "${!IMG1_LAYERS[@]}" ; do
        echo "Diffing ${IMG1_LAYERS[$I]} and ${IMG2_LAYERS[$I]}" >> "${BENCHMARK_LOG}"
        local TARGET1=$(mktemp -d)
        local TARGET2=$(mktemp -d)
        tar xf "${IMG1}" "${IMG1_LAYERS[$I]}" -O | tar zx -C "${TARGET1}"
        tar xf "${IMG2}" "${IMG2_LAYERS[$I]}" -O | tar zx -C "${TARGET2}"
        echo "Snapshot of 1st image(${TARGET1}): " >> "${BENCHMARK_LOG}"
        ls -al "${TARGET1}" >> "${BENCHMARK_LOG}" 2>&1
        echo "Snapshot of 2nd image(${TARGET2}): " >> "${BENCHMARK_LOG}"
        ls -al "${TARGET2}" >> "${BENCHMARK_LOG}" 2>&1

        # compares two snapshots without comparing timestamps
        local SUM1=$(tar -C "${TARGET1}" --mtime='1970-01-01' -c . -O | sha256sum | cut -d ' ' -f 1)
        local SUM2=$(tar -C "${TARGET2}" --mtime='1970-01-01' -c . -O | sha256sum | cut -d ' ' -f 1)
        rm -rf "${TARGET1}" "${TARGET2}"
        echo "Sums are ${SUM1} and ${SUM2}" >> "${BENCHMARK_LOG}"
        if ! [ "${SUM1}" == "${SUM2}" ] ; then
            return 1
        fi
    done

    return 0
}

if ! [ -d "${IMAGE_DEST_DIR}" ] ; then
    mkdir -p "${IMAGE_DEST_DIR}"
fi

echo '[{'
for I in "${!IMAGES[@]}" ; do
    IMAGE="${IMAGES[$I]}"
    CONTEXT="${SAMPLE_DIR}/${IMAGE}/"
    rm -f "${ORG_DEST_FILE}" "${SGZ_DEST_FILE}" "${ESGZ_DEST_FILE}" >/dev/null 2>&1
    echo '"'"${IMAGE}"'": ['
    for J in "${!MODES[@]}"; do
        MODE="${MODES[$J]}"

        # configures some parameters according to the mode
        IMAGE_SUFFIX=
        DEST_FILE=
        BUILDKIT_PATH=
        BUILDKITD_OPTS=
        if [ "${MODE}" == "legacy" ] ; then
            BUILDKIT_PATH="${BUILDKIT_MASTER}"
            if [ "${BUILDKIT_WORKER}" == "oci" ] ; then
                # uses oci worker
                BUILDKITD_OPTS="--oci-worker-snapshotter=overlayfs"
            else
                # uses containerd worker
                BUILDKITD_OPTS="--oci-worker=false --containerd-worker=true"
            fi
            IMAGE_SUFFIX="org"
            DEST_FILE="${ORG_DEST_FILE}"
        else
            BUILDKIT_PATH="${BUILDKIT_STARGZ}"
            if [ "${BUILDKIT_WORKER}" == "oci" ] ; then
                # uses oci worker
                BUILDKITD_OPTS="--oci-worker-snapshotter=stargz --oci-worker-proxy-snapshotter-address=${STARGZ_SNAPSHOTTER_ADDRESS}"
            else
                # uses containerd worker
                BUILDKITD_OPTS="--oci-worker=false --containerd-worker=true --containerd-worker-snapshotter=stargz"
            fi
            if [ "${MODE}" == "stargz" ] ; then
                IMAGE_SUFFIX="sgz"
                DEST_FILE="${SGZ_DEST_FILE}"
            else
                IMAGE_SUFFIX="esgz"
                DEST_FILE="${ESGZ_DEST_FILE}"
            fi
        fi
        echo "suffix: ${IMAGE_SUFFIX}, destination: ${DEST_FILE}, buildkit path: ${BUILDKIT_PATH}, options for buildkitd: ${BUILDKITD_OPTS}" >> "${BENCHMARK_LOG}"
        if [ "${IMAGE_SUFFIX}" == "" ] \
               || [ "${DEST_FILE}" == "" ] \
               || [ "${BUILDKIT_PATH}" == "" ] \
               || [ "${BUILDKITD_OPTS}" == "" ]; then
            echo "fatal(internal): ${IMAGE}: ${MODE}: necessary information doesn't exist for image mode" | tee -a "${BENCHMARK_LOG}"
            exit 1
        fi

        # builds specified image and measure the time to take for building
        DOCKERFILE_DIR=$(mktemp -d)
        cat "${CONTEXT}Dockerfile" \
            | sed 's/'"${DOCKERFILE_SUFFIX_PH}"'/'"${IMAGE_SUFFIX}"'/g' > "${DOCKERFILE_DIR}/Dockerfile"
        run "${BUILDKIT_PATH}" ${BUILDKITD_OPTS}
        ELAPSED=$(build "${BUILDKIT_PATH}" "${CONTEXT}" "${DOCKERFILE_DIR}" "${DEST_FILE}")

        echo -n '{ "mode" : "'"${MODE}"'", "elapsed": '"${ELAPSED}"' }'
        if ! [ ${J} -eq $(expr ${#MODES[@]} - 1) ] ; then
            echo ","
        else
            echo ""
        fi

        if [ "${TEST_ENABLE}" == "true" ] ; then
            # checks the image contents are the same as the original image
            if ! is_same_image "${DEST_FILE}" "${ORG_DEST_FILE}" ; then
                echo "error: ${IMAGE}: ${MODE}: ${DEST_FILE} and ${ORG_DEST_FILE} are different" \
                    | tee -a "${BENCHMARK_LOG}"
                exit 1
            fi
        fi
    done
    if [ ${I} -eq $(expr ${#IMAGES[@]} - 1) ] ; then
        echo "]"
    else
        echo "],"
    fi
done
echo '}]'
