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

# Preparing creds for provided user and password for private registry(only for testing purpose)
# See also: https://docs.docker.com/registry/deploying/
function prepare_creds {
    local OUTPUT="${1}"
    local REGISTRY_HOST="${2}"
    local USER="${3}"
    local PASS="${4}"
    mkdir "${OUTPUT}/auth" "${OUTPUT}/certs"
    openssl req -subj "/C=JP/ST=Remote/L=Snapshotter/O=TestEnv/OU=Integration/CN=${REGISTRY_HOST}" \
            -addext "subjectAltName = DNS:${REGISTRY_HOST}" \
            -newkey rsa:2048 -nodes -keyout "${OUTPUT}/certs/domain.key" \
            -x509 -days 365 -out "${OUTPUT}/certs/domain.crt"
    htpasswd -Bbn "${USER}" "${PASS}" > "${OUTPUT}/auth/htpasswd"
}

# Check if all snapshots logged in the specified file are prepared as remote snapshots.
# Whether a snapshot is prepared as a remote snapshot must be logged with the key
# "remote-snapshot-prepared" in JSON-formatted log.
# See also /snapshot/snapshot.go in this repo.
LOG_REMOTE_SNAPSHOT="remote-snapshot-prepared"
function check_remote_snapshots {
    local LOG_FILE="${1}"
    local REMOTE=0
    local LOCAL=0

    REMOTE=$(jq -r 'select(."'"${LOG_REMOTE_SNAPSHOT}"'" == "true")' "${LOG_FILE}" | wc -l)
    LOCAL=$(jq -r 'select(."'"${LOG_REMOTE_SNAPSHOT}"'" == "false")' "${LOG_FILE}" | wc -l)
    if [[ ${LOCAL} -gt 0 ]] ; then
        echo "some local snapshots creation have been reported (local:${LOCAL},remote:${REMOTE})"
        return 1
    elif [[ ${REMOTE} -gt 0 ]] ; then
        echo "all layers have been reported as remote snapshots (local:${LOCAL},remote:${REMOTE})"
        return 0
    else
        echo "no log for checking remote snapshot was provided; Is the log-level = debug?"
        return 1
    fi
}
