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

ARG CONTAINERD_VERSION=v1.4.0
ARG RUNC_VERSION=v1.0.0-rc92

FROM golang:1.13-buster AS golang-base

# Build containerd
FROM golang-base AS containerd-dev
ARG CONTAINERD_VERSION
RUN apt-get update -y && apt-get install -y libbtrfs-dev libseccomp-dev && \
    git clone -b ${CONTAINERD_VERSION} --depth 1 \
              https://github.com/containerd/containerd $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    GO111MODULE=off make && DESTDIR=/out/ make install

# Build runc
FROM golang-base AS runc-dev
ARG RUNC_VERSION
RUN apt-get update -y && apt-get install -y libseccomp-dev && \
    git clone -b ${RUNC_VERSION} --depth 1 \
              https://github.com/opencontainers/runc $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    GO111MODULE=off make && make install PREFIX=/out/

# Build stargz snapshotter
FROM golang-base AS snapshotter-dev
ARG SNAPSHOTTER_BUILD_FLAGS
ARG CTR_REMOTE_BUILD_FLAGS
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN cd $GOPATH/src/github.com/containerd/stargz-snapshotter && \
    PREFIX=/out/ GO_BUILD_FLAGS=${SNAPSHOTTER_BUILD_FLAGS} make containerd-stargz-grpc && \
    PREFIX=/out/ GO_BUILD_FLAGS=${CTR_REMOTE_BUILD_FLAGS} make ctr-remote

# Binaries for release
FROM scratch AS release-binaries
COPY --from=snapshotter-dev /out/* /

# Base image which contains containerd with default snapshotter
# `docker-ce-cli` is used only for users to `docker login` to registries (e.g. DockerHub)
# with configuring ~/.docker/config.json
FROM golang-base AS containerd-base
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse \
                                 apt-transport-https gnupg2 software-properties-common && \
    curl -fsSL https://download.docker.com/linux/debian/gpg | apt-key add - && \
    add-apt-repository \
      "deb [arch=amd64] https://download.docker.com/linux/debian $(lsb_release -cs) stable" && \
    apt-get update -y && apt-get --no-install-recommends install -y docker-ce-cli
COPY --from=containerd-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/

# Base image which contains containerd with stargz snapshotter
FROM containerd-base AS snapshotter-base
COPY --from=snapshotter-dev /out/* /usr/local/bin/
RUN ln -s /usr/local/bin/ctr-remote /usr/local/bin/ctr

# Image which can be used as all-in-one single node demo environment
FROM snapshotter-base AS cind
COPY ./script/config/ /
COPY ./script/cind/ /
VOLUME /var/lib/containerd
VOLUME /var/lib/containerd-stargz-grpc
VOLUME /run/containerd-stargz-grpc
ENV CONTAINERD_SNAPSHOTTER=stargz
ENTRYPOINT [ "/entrypoint.sh" ]

# Image which can be used as containerized `ctr-remote images optimize` command
FROM ubuntu:20.04 AS oind

RUN apt-get update -y && \
    apt-get --no-install-recommends install -y fuse runc ca-certificates

COPY --from=snapshotter-dev /out/ctr-remote /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/

ENTRYPOINT [ "/usr/local/bin/ctr-remote", "images", "optimize" ]

# Image which can be used as a node image for KinD
FROM kindest/node:v1.19.0
COPY --from=containerd-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=snapshotter-dev /out/* /usr/local/bin/
COPY ./script/config/ /
RUN apt-get update -y && apt-get install --no-install-recommends -y fuse && \
    systemctl enable stargz-snapshotter
ENTRYPOINT [ "/usr/local/bin/entrypoint", "/sbin/init" ]
