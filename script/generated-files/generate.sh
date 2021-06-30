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
REPO="${CONTEXT}../../"

source "${REPO}/script/util/utils.sh"

GOBASE_VERSION=$(go_base_version "${REPO}/Dockerfile")

# TODO: get the following versions from go.mod once we add them.
PROTOC_VERSION=3.17.3
GOGO_VERSION=v1.3.2

COMMAND="${1}"
if [ "${COMMAND}" != "update" ] && [ "${COMMAND}" != "validate" ] ; then
    echo "either of \"update\" or \"validate\" must be specified"
    exit 1
fi

TMP_CONTEXT=$(mktemp -d)
GENERATED_FILES=$(mktemp -d)
function cleanup {
    local ORG_EXIT_CODE="${1}"
    rm -rf "${GENERATED_FILES}" || true
    rm -rf "${TMP_CONTEXT}" || true
    exit "${ORG_EXIT_CODE}"
}
trap 'cleanup "$?"' EXIT SIGHUP SIGINT SIGQUIT SIGTERM

cat <<EOF > "${TMP_CONTEXT}/Dockerfile"
FROM golang:${GOBASE_VERSION} AS golang-base

ARG PROTOC_VERSION=${PROTOC_VERSION}
ARG GOGO_VERSION=${GOGO_VERSION}
ARG TARGETOS TARGETARCH

RUN apt-get update && apt-get --no-install-recommends install -y unzip && \
    PARCH=\$(echo \${TARGETARCH} | sed -e 's/amd64/x86_64/' -e 's/arm64/aarch_64/') && \
    wget -O /tmp/protoc.zip \
         https://github.com/google/protobuf/releases/download/v\${PROTOC_VERSION}/protoc-\${PROTOC_VERSION}-\${TARGETOS}-\${PARCH}.zip && \
    unzip /tmp/protoc.zip -d /usr/local && \
    go install github.com/gogo/protobuf/protoc-gen-gogo@\$GOGO_VERSION

WORKDIR /go/src/github.com/containerd/stargz-snapshotter

FROM golang-base AS generate
COPY . .
RUN git add -A && \
    go generate -v ./... && \
    mkdir /generated-files && \
    git ls-files -m --others -- **/*.pb.go | tar -cf - --files-from - | tar -C /generated-files -xf -

FROM scratch AS update
COPY --from=generate /generated-files /

FROM golang-base AS validate
COPY . .
RUN git add -A && \
    go generate -v ./... && \
    DIFFS=\$(git status --porcelain -- **/*.pb.go 2>/dev/null) ; \
    if [ "\${DIFFS}" ]; then \
      echo "Unexpected generated files; Please run \"make generate\"" && \
      git diff && \
      echo \${DIFFS} && \
      exit 1 ; \
    else \
      echo OK ; \
    fi
EOF

case ${COMMAND} in
    update)
        DOCKER_BUILDKIT=1 docker build --progress=plain --target update -f "${TMP_CONTEXT}/Dockerfile" -o - "${REPO}" | tar -C "${GENERATED_FILES}/" -x
        if ! [ -z "$(ls -A ${GENERATED_FILES}/)" ]; then cp -R "${GENERATED_FILES}/"* "${REPO}" ; fi
        ;;
    validate)
        DOCKER_BUILDKIT=1 docker build --progress=plain --target validate -f "${TMP_CONTEXT}/Dockerfile" "${REPO}"
        ;;
esac
