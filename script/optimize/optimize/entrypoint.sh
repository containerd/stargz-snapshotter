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

REGISTRY_HOST=registry-optimize
DUMMYUSER=dummyuser
DUMMYPASS=dummypass
ORG_IMAGE_TAG="${REGISTRY_HOST}:5000/test:org$(date '+%M%S')"
OPT_IMAGE_TAG="${REGISTRY_HOST}:5000/test:opt$(date '+%M%S')"
NOOPT_IMAGE_TAG="${REGISTRY_HOST}:5000/test:noopt$(date '+%M%S')"
TOC_JSON_DIGEST_ANNOTATION="containerd.io/snapshot/stargz/toc.digest"
NETWORK_MOUNT_TEST_ORG_IMAGE_TAG="ghcr.io/stargz-containers/centos:8-test"
REMOTE_SNAPSHOTTER_SOCKET=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock

RETRYNUM=100
RETRYINTERVAL=1
TIMEOUTSEC=180
function retry {
    local SUCCESS=false
    for i in $(seq ${RETRYNUM}) ; do
        if eval "timeout ${TIMEOUTSEC} ${@}" ; then
            SUCCESS=true
            break
        fi
        echo "Fail(${i}). Retrying..."
        sleep ${RETRYINTERVAL}
    done
    if [ "${SUCCESS}" == "true" ] ; then
        return 0
    else
        return 1
    fi
}

function prepare_context {
    local CONTEXT_DIR="${1}"
    cat <<EOF > "${CONTEXT_DIR}/Dockerfile"
FROM scratch

COPY ./a.txt ./b.txt accessor /
COPY ./c.txt ./d.txt /
COPY ./e.txt /

ENTRYPOINT ["/accessor"]

EOF
    for SAMPLE in "a" "b" "c" "d" "e" ; do
        echo "${SAMPLE}" > "${CONTEXT_DIR}/${SAMPLE}.txt"
    done
    mkdir -p "${GOPATH}/src/test/test" && \
        cat <<'EOF' > "${GOPATH}/src/test/test/main.go"
package main

import (
	"os"
)

func main() {
	targets := []string{"/a.txt", "/c.txt"}
	for _, t := range targets {
		f, err := os.Open(t)
		if err != nil {
			panic("failed to open file")
		}
		f.Close()
	}
}
EOF
    GO111MODULE=off go build -ldflags '-extldflags "-static"' -o "${CONTEXT_DIR}/accessor" "${GOPATH}/src/test/test"
}

function validate_toc_json {
    local MANIFEST=${1}
    local LAYER_NUM=${2}
    local LAYER_TAR=${3}

    TOCJSON_ANNOTATION="$(cat ${MANIFEST} | jq -r '.layers['"${LAYER_NUM}"'].annotations."'${TOC_JSON_DIGEST_ANNOTATION}'"')"
    TOCJSON_DIGEST=$(tar -xOf "${LAYER_TAR}" "stargz.index.json" | sha256sum | sed -E 's/([^ ]*).*/sha256:\1/g')

    if [ "${TOCJSON_ANNOTATION}" != "${TOCJSON_DIGEST}" ] ; then
        echo "Invalid TOC JSON (layer:${LAYER_NUM}): want ${TOCJSON_ANNOTATION}; got: ${TOCJSON_DIGEST}"
        return 1
    fi

    echo "Valid TOC JSON (layer:${LAYER_NUM}) ${TOCJSON_ANNOTATION} == ${TOCJSON_DIGEST}"
    return 0
}

function check_optimization {
    local TARGET=${1}

    LOCAL_WORKING_DIR="${WORKING_DIR}/$(date '+%H%M%S')"
    mkdir "${LOCAL_WORKING_DIR}"
    docker pull "${TARGET}" && docker save "${TARGET}" | tar xv -C "${LOCAL_WORKING_DIR}"
    LAYERS="$(cat "${LOCAL_WORKING_DIR}/manifest.json" | jq -r '.[0].Layers[]')"

    echo "Checking layers..."
    GOTNUM=0
    for L in ${LAYERS}; do
        tar --list -f "${LOCAL_WORKING_DIR}/${L}" | tee "${LOCAL_WORKING_DIR}/${GOTNUM}"
        ((GOTNUM+=1))
    done
    WANTNUM=0
    for W in "${@:2}"; do
        cp "${W}" "${LOCAL_WORKING_DIR}/${WANTNUM}-want"
        ((WANTNUM+=1))
    done
    if [ "${GOTNUM}" != "${WANTNUM}" ] ; then
        echo "invalid numbers of layers ${GOTNUM}; want ${WANTNUM}"
        return 1
    fi
    for ((I=0; I < WANTNUM; I++)) ; do
        echo "Validating tarball contents of layer ${I}..."
        diff "${LOCAL_WORKING_DIR}/${I}" "${LOCAL_WORKING_DIR}/${I}-want"
    done
    crane manifest "${TARGET}" | tee "${LOCAL_WORKING_DIR}/dist-manifest.json" && echo ""
    INDEX=0
    for L in ${LAYERS}; do
        echo "Validating TOC JSON digest of layer ${INDEX}..."
        validate_toc_json "${LOCAL_WORKING_DIR}/dist-manifest.json" \
                          "${INDEX}" \
                          "${LOCAL_WORKING_DIR}/${L}"
        ((INDEX+=1))
    done

    return 0
}

echo "Connecting to the docker server..."
retry ls /docker/client/cert.pem /docker/client/ca.pem
mkdir -p /root/.docker/ && cp /docker/client/* /root/.docker/
retry docker version

echo "Logging into the registry..."
cp /auth/certs/domain.crt /usr/local/share/ca-certificates
update-ca-certificates
retry docker login "${REGISTRY_HOST}:5000" -u "${DUMMYUSER}" -p "${DUMMYPASS}"

echo "Building sample image for testing..."
CONTEXT_DIR=$(mktemp -d)
prepare_context "${CONTEXT_DIR}"

echo "Preparing sample image..."
tar zcv -C "${CONTEXT_DIR}" . \
    | docker build -t "${ORG_IMAGE_TAG}" - \
    && docker push "${ORG_IMAGE_TAG}"

echo "Loading original image"
containerd --log-level debug &
retry ctr version
ctr i pull "${NETWORK_MOUNT_TEST_ORG_IMAGE_TAG}"
ctr i pull -u "${DUMMYUSER}:${DUMMYPASS}" "${ORG_IMAGE_TAG}"

echo "Checking optimized image..."
WORKING_DIR=$(mktemp -d)
PREFIX=/tmp/out/ make clean
PREFIX=/tmp/out/ GO_BUILD_FLAGS="-race" make ctr-remote # Check data race
/tmp/out/ctr-remote ${OPTIMIZE_COMMAND} -entrypoint='[ "/accessor" ]' "${ORG_IMAGE_TAG}" "${OPT_IMAGE_TAG}"
ctr i push -u "${DUMMYUSER}:${DUMMYPASS}" "${OPT_IMAGE_TAG}" || true
cat <<EOF > "${WORKING_DIR}/0-want"
accessor
a.txt
.prefetch.landmark
b.txt
stargz.index.json
EOF

cat <<EOF > "${WORKING_DIR}/1-want"
c.txt
.prefetch.landmark
d.txt
stargz.index.json
EOF

cat <<EOF > "${WORKING_DIR}/2-want"
.no.prefetch.landmark
e.txt
stargz.index.json
EOF

check_optimization "${OPT_IMAGE_TAG}" \
                   "${WORKING_DIR}/0-want" \
                   "${WORKING_DIR}/1-want" \
                   "${WORKING_DIR}/2-want"

echo "Checking non-optimized image..."
/tmp/out/ctr-remote ${NO_OPTIMIZE_COMMAND} "${ORG_IMAGE_TAG}" "${NOOPT_IMAGE_TAG}"
ctr i push -u "${DUMMYUSER}:${DUMMYPASS}" "${NOOPT_IMAGE_TAG}" || true
cat <<EOF > "${WORKING_DIR}/0-want"
.no.prefetch.landmark
a.txt
accessor
b.txt
stargz.index.json
EOF

cat <<EOF > "${WORKING_DIR}/1-want"
.no.prefetch.landmark
c.txt
d.txt
stargz.index.json
EOF

cat <<EOF > "${WORKING_DIR}/2-want"
.no.prefetch.landmark
e.txt
stargz.index.json
EOF

check_optimization "${NOOPT_IMAGE_TAG}" \
                   "${WORKING_DIR}/0-want" \
                   "${WORKING_DIR}/1-want" \
                   "${WORKING_DIR}/2-want"

# Test networking & mounting work

# Make bridge plugin manipulate iptables instead of nftables as this test runs
# in a Docker container that network is configured with iptables.
# c.f. https://github.com/moby/moby/issues/26824
update-alternatives --set iptables /usr/sbin/iptables-legacy

# Install CNI plugins and configs to the customized paths (not /opt/cni/bin and /etc/cni/net.d)
mkdir /tmp/cni/ /tmp/bin/
curl -Ls https://github.com/containernetworking/plugins/releases/download/v0.8.7/cni-plugins-linux-amd64-v0.8.7.tgz | tar xzv -C /tmp/bin
cat <<'EOF' > /tmp/cni/test.conflist
{
  "cniVersion": "0.4.0",
  "name": "test",
  "plugins" : [{
    "type": "bridge",
    "bridge": "test0",
    "isDefaultGateway": true,
    "forceAddress": false,
    "ipMasq": true,
    "hairpinMode": true,
    "ipam": {
      "type": "host-local",
      "subnet": "10.10.0.0/16"
    }
  },
  {
    "type": "loopback"
  }]
}
EOF

# Try to connect to the internet from the container
TESTDIR=$(mktemp -d)
/tmp/out/ctr-remote ${OPTIMIZE_COMMAND} \
                    --period=20 \
                    --cni \
                    --cni-plugin-conf-dir='/tmp/cni' \
                    --cni-plugin-dir='/tmp/bin' \
                    --add-hosts='testhost:1.2.3.4,test2:5.6.7.8' \
                    --dns-nameservers='8.8.8.8' \
                    --mount="type=bind,src=${TESTDIR},dst=/mnt,options=bind" \
                    --entrypoint='[ "/bin/bash", "-c" ]' \
                    --args='[ "curl example.com > /mnt/result_page && ip a show dev eth0 ; echo -n $? > /mnt/if_exists && ip a > /mnt/if_info && cat /etc/hosts > /mnt/hosts" ]' \
                    "${NETWORK_MOUNT_TEST_ORG_IMAGE_TAG}" "${REGISTRY_HOST}:5000/test:1"

# Check if all contents are successfuly passed
if ! [ -f "${TESTDIR}/if_exists" ] || \
        ! [ -f "${TESTDIR}/result_page" ] || \
        ! [ -f "${TESTDIR}/if_info" ] || \
        ! [ -f "${TESTDIR}/hosts" ]; then
    echo "the result files not found; bind-mount might not work"
    exit 1
fi

# Check if /etc/hosts contains expected contents
if [ "$(cat ${TESTDIR}/hosts | grep testhost | sed -E 's/([0-9.]*).*/\1/')" != "1.2.3.4" ] || \
       [ "$(cat ${TESTDIR}/hosts | grep test2 | sed -E 's/([0-9.]*).*/\1/')" != "5.6.7.8" ]; then
    echo "invalid contents in /etc/hosts"
    cat "${TESTDIR}/hosts"
    exit 1
fi
echo "hosts configured:"
cat "${TESTDIR}/hosts"

# Check if the interface is created by the bridge plugin
if [ "$(cat ${TESTDIR}/if_exists)" != "0" ] ; then
    echo "interface didn't configured:"
    cat "${TESTDIR}/if_exists"
    echo "interface info:"
    cat "${TESTDIR}/if_info"
    exit 1
fi
echo "Interface created:"
cat "${TESTDIR}/if_info"

# Check if the contents are downloaded from the internet
SAMPLE_PAGE=$(mktemp)
curl example.com > "${SAMPLE_PAGE}"
if ! [ -s "${SAMPLE_PAGE}" ] ; then
    echo "sample page file is empty; failed to get the contents of example.com; check the internet connection"
    exit 1
fi
echo "sample contents of example.com"
cat "${SAMPLE_PAGE}"
SAMPLE_PAGE_SHA256=$(cat "${SAMPLE_PAGE}" | sha256sum | sed -E 's/([^ ]*).*/sha256:\1/g')
RESULT_PAGE_SHA256=$(cat "${TESTDIR}/result_page" | sha256sum | sed -E 's/([^ ]*).*/sha256:\1/g')
if [ "${SAMPLE_PAGE_SHA256}" != "${RESULT_PAGE_SHA256}" ] ; then
    echo "failed to get expected contents from the internet, inside the container: ${SAMPLE_PAGE_SHA256} != ${RESULT_PAGE_SHA256}"
    echo "got contetns:"
    cat "${TESTDIR}/result_page"
    exit 1
fi
echo "expected contents successfly downloaded from the internet, in the container. contents:"
cat "${TESTDIR}/result_page"

exit 0
