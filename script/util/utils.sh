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
            -newkey rsa:2048 -nodes -keyout "${OUTPUT}/certs/domain.key" \
            -x509 -days 365 -out "${OUTPUT}/certs/domain.crt"
    docker run --entrypoint htpasswd registry:2 -Bbn "${USER}" "${PASS}" > "${OUTPUT}/auth/htpasswd"
}
