#!/bin/bash

if [ "${1}" == "" ]; then
    echo "Repository path must be provided."
    exit 1
fi

if [ "${2}" == "" ]; then
    echo "Authentication-replated directory path must be provided."
    exit 1
fi

if [ "${3}" == "" ]; then
    echo "Temp dir for /var/lib/rs must be provided."
    exit 1
fi

REPO="${1}"
AUTH="${2}"
RS_DIR="${3}"

cat <<EOF
version: "3"
services:
  testenv_integration:
    build:
      context: "${REPO}/script/make_wrapper/containerd"
      dockerfile: Dockerfile
    container_name: testenv_integration
    privileged: true
    working_dir: /go/src/github.com/ktock/remote-snapshotter
    entrypoint: ./script/make_wrapper/containerd/entrypoint.sh
    environment:
    - GO111MODULE=off
    - NO_PROXY=127.0.0.1,localhost,registry_integration:5000
    - HTTP_PROXY=${HTTP_PROXY}
    - HTTPS_PROXY=${HTTPS_PROXY}
    - http_proxy=${http_proxy}
    - https_proxy=${https_proxy}
    tmpfs:
    - /var/lib/containerd
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:/go/src/github.com/ktock/remote-snapshotter:ro"
    - ${AUTH}:/auth
    - "${RS_DIR}:/var/lib/rs:rshared"
  registry:
    image: registry:2
    container_name: registry_integration
    environment:
    - HTTP_PROXY=${HTTP_PROXY}
    - HTTPS_PROXY=${HTTPS_PROXY}
    - http_proxy=${http_proxy}
    - https_proxy=${https_proxy}
    - REGISTRY_AUTH=htpasswd
    - REGISTRY_AUTH_HTPASSWD_REALM="Registry Realm"
    - REGISTRY_AUTH_HTPASSWD_PATH=/auth/auth/htpasswd
    - REGISTRY_HTTP_TLS_CERTIFICATE=/auth/certs/domain.crt
    - REGISTRY_HTTP_TLS_KEY=/auth/certs/domain.key
    volumes:
    - ${AUTH}:/auth
  remote_snapshotter_integration:
    build:
      context: "${REPO}/script/make_wrapper/rs"
      dockerfile: Dockerfile
    container_name: remote_snapshotter_integration
    privileged: true
    working_dir: /go/src/github.com/ktock/remote-snapshotter
    entrypoint: ./script/make_wrapper/rs/entrypoint.sh
    environment:
    - GO111MODULE=off
    - NO_PROXY=127.0.0.1,localhost,registry_integration:5000
    - HTTP_PROXY=${HTTP_PROXY}
    - HTTPS_PROXY=${HTTPS_PROXY}
    - http_proxy=${http_proxy}
    - https_proxy=${https_proxy}
    tmpfs:
    - /tmp:exec,mode=777
    volumes:
    - "${REPO}:/go/src/github.com/ktock/remote-snapshotter:ro"
    - "${AUTH}:/auth"
    - "${RS_DIR}:/var/lib/rs:rshared"
    - /dev/fuse:/dev/fuse
EOF
