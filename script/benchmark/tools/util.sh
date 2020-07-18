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

PERCENTILE="${BENCHMARK_PERCENTILE:-}"
if [ "${PERCENTILE}" == "" ] ; then
    PERCENTILE="95" # 95 percentile by default
fi

# samples_num functions returns the number of samples which are identified by
# image name, mode and operation(pull, create, run) in the given JSON raw data.
function samples_num {
    local JSONFILE="${1}"
    local IMAGE="${2}"
    local MODE="${3}"
    local TARGET="${4}"

    cat "${JSONFILE}" \
        | jq -r '.[] | select(.repo=="'"${IMAGE}"'" and .mode=="'"${MODE}"'")."'"${TARGET}"'"' \
        | wc -l
}

# min_samples function returns the minimal number of samples which are identified by
# image name, mode and operation(pull, create, run) in the given JSON raw data.
function min_samples {
    local JSONFILE="${1}"
    local IMAGE="${2}"
    local MODE="${3}"

    PULLSAMPLES=$(samples_num "${JSONFILE}" "${IMGNAME}" "${MODE}" "elapsed_pull")
    CREATESAMPLES=$(samples_num "${JSONFILE}" "${IMGNAME}" "${MODE}" "elapsed_create")
    RUNSAMPLES=$(samples_num "${JSONFILE}" "${IMGNAME}" "${MODE}" "elapsed_run")
    echo $(echo "${PULLSAMPLES} ${CREATESAMPLES} ${RUNSAMPLES}" | tr ' ' '\n' | sort -n | head -1)
}

# percentile function returns the specified percentile value relying on numpy.
# See also: https://numpy.org/doc/stable/reference/generated/numpy.percentile.html
CALCTEMP=$(mktemp)
function percentile {
    local JSONFILE="${1}"
    local SAMPLES="${2}"
    local IMAGE="${3}"
    local MODE="${4}"
    local TARGET="${5}"

    cat "${JSONFILE}" \
        | jq -r '.[] | select(.repo=="'"${IMAGE}"'" and .mode=="'"${MODE}"'")."'"${TARGET}"'"' \
        | sort -R | head -n "${SAMPLES}" | sort -n > "${CALCTEMP}"
    local PYTHON_BIN=
    if which python &> /dev/null ; then
        PYTHON_BIN=python
    elif which python3 &> /dev/null ; then
        # Try also with python3
        PYTHON_BIN=python3
    else
        echo "Python not found"
        exit 1
    fi
    cat <<EOF | "${PYTHON_BIN}"
import numpy as np
f = open('${CALCTEMP}', 'r')
arr = []
for line in f.readlines():
    arr.append(float(line))
f.close()
print(np.percentile(a=np.array(arr), q=${PERCENTILE}, interpolation='linear'))
EOF
}
