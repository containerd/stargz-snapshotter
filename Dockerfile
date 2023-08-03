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

ARG CONTAINERD_VERSION=v1.7.2
ARG RUNC_VERSION=v1.1.7
ARG CNI_PLUGINS_VERSION=v1.3.0
ARG NERDCTL_VERSION=1.4.0

ARG PODMAN_VERSION=v4.5.0
ARG CRIO_VERSION=v1.27.0
ARG CONMON_VERSION=v2.1.7
ARG COMMON_VERSION=v0.53.0
ARG CRIO_TEST_PAUSE_IMAGE_NAME=registry.k8s.io/pause:3.6

ARG CONTAINERIZED_SYSTEMD_VERSION=v0.1.1
ARG SLIRP4NETNS_VERSION=v1.2.0

# Used in CI
ARG CRI_TOOLS_VERSION=v1.27.0

# Legacy builder that doesn't support TARGETARCH should set this explicitly using --build-arg.
# If TARGETARCH isn't supported by the builder, the default value is "amd64".

FROM golang:1.20.7-bullseye AS golang-base

# Build containerd
FROM golang-base AS containerd-dev
ARG CONTAINERD_VERSION
RUN apt-get update -y && apt-get install -y libbtrfs-dev libseccomp-dev && \
    git clone -b ${CONTAINERD_VERSION} --depth 1 \
              https://github.com/containerd/containerd $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    make && DESTDIR=/out/ PREFIX= make install

# Build containerd with builtin stargz snapshotter
FROM golang-base AS containerd-snapshotter-dev
ARG CONTAINERD_VERSION
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN apt-get update -y && apt-get install -y libbtrfs-dev libseccomp-dev && \
    git clone -b ${CONTAINERD_VERSION} --depth 1 \
      https://github.com/containerd/containerd $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    echo 'require github.com/containerd/stargz-snapshotter v0.0.0' >> go.mod && \
    echo 'replace github.com/containerd/stargz-snapshotter => '$GOPATH'/src/github.com/containerd/stargz-snapshotter' >> go.mod && \
    echo 'replace github.com/containerd/stargz-snapshotter/estargz => '$GOPATH'/src/github.com/containerd/stargz-snapshotter/estargz' >> go.mod && \
    # recent containerd requires to update api/go.mod and integration/client/go.mod as well.
    if [ -f api/go.mod ] ; then \
      echo 'replace github.com/containerd/stargz-snapshotter => '$GOPATH'/src/github.com/containerd/stargz-snapshotter' >> api/go.mod && \
      echo 'replace github.com/containerd/stargz-snapshotter/estargz => '$GOPATH'/src/github.com/containerd/stargz-snapshotter/estargz' >> api/go.mod ; \
    fi && \
    if [ -f integration/client/go.mod ] ; then \
      echo 'replace github.com/containerd/stargz-snapshotter => '$GOPATH'/src/github.com/containerd/stargz-snapshotter' >> integration/client/go.mod && \
      echo 'replace github.com/containerd/stargz-snapshotter/estargz => '$GOPATH'/src/github.com/containerd/stargz-snapshotter/estargz' >> integration/client/go.mod ; \
    fi && \
    echo 'package main \nimport _ "github.com/containerd/stargz-snapshotter/service/plugin"' > cmd/containerd/builtins_stargz_snapshotter.go && \
    make vendor && make && DESTDIR=/out/ PREFIX= make install

# Build runc
FROM golang-base AS runc-dev
ARG RUNC_VERSION
RUN apt-get update -y && apt-get install -y libseccomp-dev && \
    git clone -b ${RUNC_VERSION} --depth 1 \
              https://github.com/opencontainers/runc $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    make && make install PREFIX=/out/

# Build stargz snapshotter
FROM golang-base AS snapshotter-dev
ARG TARGETARCH
ARG GOARM
ARG SNAPSHOTTER_BUILD_FLAGS
ARG CTR_REMOTE_BUILD_FLAGS
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN cd $GOPATH/src/github.com/containerd/stargz-snapshotter && \
    PREFIX=/out/ GOARCH=${TARGETARCH:-amd64} GO_BUILD_FLAGS=${SNAPSHOTTER_BUILD_FLAGS} make containerd-stargz-grpc && \
    PREFIX=/out/ GOARCH=${TARGETARCH:-amd64} GO_BUILD_FLAGS=${CTR_REMOTE_BUILD_FLAGS} make ctr-remote

# Build stargz store
FROM golang-base AS stargz-store-dev
ARG TARGETARCH
ARG GOARM
ARG SNAPSHOTTER_BUILD_FLAGS
ARG CTR_REMOTE_BUILD_FLAGS
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN cd $GOPATH/src/github.com/containerd/stargz-snapshotter && \
    PREFIX=/out/ GOARCH=${TARGETARCH:-amd64} GO_BUILD_FLAGS=${SNAPSHOTTER_BUILD_FLAGS} make stargz-store

# Build podman
FROM golang-base AS podman-dev
ARG PODMAN_VERSION
RUN apt-get update -y && apt-get install -y libseccomp-dev libgpgme-dev && \
    git clone https://github.com/containers/podman $GOPATH/src/github.com/containers/podman && \
    cd $GOPATH/src/github.com/containers/podman && \
    git checkout ${PODMAN_VERSION} && \
    make && make install PREFIX=/out/

# Build CRI-O
FROM golang-base AS cri-o-dev
ARG CRIO_VERSION
RUN apt-get update -y && apt-get install -y libseccomp-dev libgpgme-dev && \
    git clone https://github.com/cri-o/cri-o $GOPATH/src/github.com/cri-o/cri-o && \
    cd $GOPATH/src/github.com/cri-o/cri-o && \
    git checkout ${CRIO_VERSION} && \
    make && make install PREFIX=/out/ && \
    curl -sSL --output /out/crio.service https://raw.githubusercontent.com/cri-o/cri-o/${CRIO_VERSION}/contrib/systemd/crio.service

# Build conmon
FROM golang-base AS conmon-dev
ARG CONMON_VERSION
RUN apt-get update -y && apt-get install -y gcc git libc6-dev libglib2.0-dev pkg-config make libseccomp-dev && \
    git clone -b ${CONMON_VERSION} --depth 1 \
              https://github.com/containers/conmon $GOPATH/src/github.com/containers/conmon && \
    cd $GOPATH/src/github.com/containers/conmon && \
    mkdir /out/ && make && make install PREFIX=/out/

# Get seccomp.json for Podman/CRI-O
FROM golang-base AS containers-common-dev
ARG COMMON_VERSION
RUN git clone https://github.com/containers/common $GOPATH/src/github.com/containers/common && \
    cd $GOPATH/src/github.com/containers/common && \
    git checkout ${COMMON_VERSION} && mkdir /out/ && cp pkg/seccomp/seccomp.json /out/

# Binaries for release
FROM scratch AS release-binaries
COPY --from=snapshotter-dev /out/* /
COPY --from=stargz-store-dev /out/* /

# Base image which contains containerd with default snapshotter
FROM golang-base AS containerd-base
ARG TARGETARCH
ARG NERDCTL_VERSION
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse3 && \
    curl -sSL --output /tmp/nerdctl.tgz https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH:-amd64}.tar.gz && \
    tar zxvf /tmp/nerdctl.tgz -C /usr/local/bin && \
    rm -f /tmp/nerdctl.tgz
COPY --from=containerd-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/

# Base image which contains containerd with stargz snapshotter
FROM containerd-base AS snapshotter-base
COPY --from=snapshotter-dev /out/* /usr/local/bin/
RUN ln -s /usr/local/bin/ctr-remote /usr/local/bin/ctr

# Base image which contains containerd with builtin stargz snapshotter
FROM golang-base AS containerd-snapshotter-base
ARG TARGETARCH
ARG NERDCTL_VERSION
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse3 && \
    curl -sSL --output /tmp/nerdctl.tgz https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH:-amd64}.tar.gz && \
    tar zxvf /tmp/nerdctl.tgz -C /usr/local/bin && \
    rm -f /tmp/nerdctl.tgz
COPY --from=containerd-snapshotter-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/
COPY --from=snapshotter-dev /out/ctr-remote /usr/local/bin/
RUN ln -s /usr/local/bin/ctr-remote /usr/local/bin/ctr

# Base image which contains podman with stargz-store
FROM golang-base AS podman-base
ARG TARGETARCH
ARG CNI_PLUGINS_VERSION
ARG PODMAN_VERSION
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse3 libgpgme-dev \
                         iptables libyajl-dev && \
    # Make CNI plugins manipulate iptables instead of nftables
    # as this test runs in a Docker container that network is configured with iptables.
    # c.f. https://github.com/moby/moby/issues/26824
    update-alternatives --set iptables /usr/sbin/iptables-legacy && \
    mkdir -p /etc/containers /etc/cni/net.d /opt/cni/bin && \
    curl -qsSL https://raw.githubusercontent.com/containers/podman/${PODMAN_VERSION}/cni/87-podman-bridge.conflist | tee /etc/cni/net.d/87-podman-bridge.conflist && \
    curl -Ls https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH:-amd64}-${CNI_PLUGINS_VERSION}.tgz | tar xzv -C /opt/cni/bin

COPY --from=podman-dev /out/bin/* /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/
COPY --from=conmon-dev /out/bin/* /usr/local/bin/
COPY --from=containers-common-dev /out/seccomp.json /usr/share/containers/
COPY --from=stargz-store-dev /out/* /usr/local/bin/

# Image for testing rootless Podman with Stargz Store.
# This takes the same approach as nerdctl CI: https://github.com/containerd/nerdctl/blob/6341c8320984f7148b92dd33472d8eaca6dba756/Dockerfile#L302-L326
FROM podman-base AS podman-rootless
ARG CONTAINERIZED_SYSTEMD_VERSION
ARG SLIRP4NETNS_VERSION
RUN apt-get update -y && apt-get install -y \
                         systemd systemd-sysv dbus dbus-user-session \
                         openssh-server openssh-client uidmap
RUN curl -o /usr/local/bin/slirp4netns --fail -L https://github.com/rootless-containers/slirp4netns/releases/download/${SLIRP4NETNS_VERSION}/slirp4netns-$(uname -m) && \
    chmod +x /usr/local/bin/slirp4netns && \
    curl -L -o /docker-entrypoint.sh https://raw.githubusercontent.com/AkihiroSuda/containerized-systemd/${CONTAINERIZED_SYSTEMD_VERSION}/docker-entrypoint.sh && \
    chmod +x /docker-entrypoint.sh && \
    curl -L -o /etc/containers/policy.json https://raw.githubusercontent.com/containers/skopeo/master/default-policy.json
# storage.conf plugs Stargz Store into Podman as an Additional Layer Store
COPY ./script/podman/config/storage.conf /home/rootless/.config/containers/storage.conf
# Stargz Store systemd service for rootless Podman
COPY ./script/podman/config/podman-rootless-stargz-store.service /home/rootless/.config/systemd/user/
# test-podman-rootless.sh logins to the user via SSH
COPY ./script/podman/config/test-podman-rootless.sh /test-podman-rootless.sh
RUN ssh-keygen -q -t rsa -f /root/.ssh/id_rsa -N '' && \
    useradd -m -s /bin/bash rootless && \
    mkdir -p -m 0700 /home/rootless/.ssh && \
    cp -a /root/.ssh/id_rsa.pub /home/rootless/.ssh/authorized_keys && \
    mkdir -p /home/rootless/.local/share /home/rootless/.local/share/stargz-store/store && \
    chown -R rootless:rootless /home/rootless
VOLUME /home/rootless/.local/share

ENTRYPOINT ["/docker-entrypoint.sh", "/test-podman-rootless.sh"]
CMD ["/bin/bash", "--login", "-i"]

# Image which can be used for interactive demo environment
FROM containerd-base AS demo
ARG CNI_PLUGINS_VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y iptables && \
    # Make CNI plugins manipulate iptables instead of nftables
    # as this test runs in a Docker container that network is configured with iptables.
    # c.f. https://github.com/moby/moby/issues/26824
    update-alternatives --set iptables /usr/sbin/iptables-legacy && \
    mkdir -p /opt/cni/bin && \
    curl -Ls https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH:-amd64}-${CNI_PLUGINS_VERSION}.tgz | tar xzv -C /opt/cni/bin

# Image which can be used as a node image for KinD (containerd with builtin snapshotter)
FROM kindest/node:v1.27.3 AS kind-builtin-snapshotter
COPY --from=containerd-snapshotter-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=snapshotter-dev /out/ctr-remote /usr/local/bin/
COPY ./script/config/ /
RUN apt-get update -y && apt-get install --no-install-recommends -y fuse3
ENTRYPOINT [ "/usr/local/bin/kind-entrypoint.sh", "/usr/local/bin/entrypoint", "/sbin/init" ]

# Image for testing CRI-O with Stargz Store.
# NOTE: This cannot be used for the node image of KinD.
FROM ubuntu:23.04 AS crio-stargz-store
ARG CNI_PLUGINS_VERSION
ARG CRIO_TEST_PAUSE_IMAGE_NAME
ENV container docker
RUN apt-get update -y && apt-get install --no-install-recommends -y \
                         ca-certificates fuse3 libgpgme-dev libglib2.0-dev curl \
                         iptables conntrack systemd systemd-sysv && \
    DEBIAN_FRONTEND=noninteractive apt-get install --no-install-recommends -y tzdata && \
    # Make CNI plugins manipulate iptables instead of nftables
    # as this test runs in a Docker container that network is configured with iptables.
    # c.f. https://github.com/moby/moby/issues/26824
    update-alternatives --set iptables /usr/sbin/iptables-legacy && \
    mkdir -p /opt/cni/bin && \
    curl -sSL https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH:-amd64}-${CNI_PLUGINS_VERSION}.tgz | tar xzv -C /opt/cni/bin && \
    echo ${CRIO_TEST_PAUSE_IMAGE_NAME} > /pause_name && \
    mkdir -p /etc/sysconfig && \
    echo CRIO_RUNTIME_OPTIONS=--pause-image=${CRIO_TEST_PAUSE_IMAGE_NAME} > /etc/sysconfig/crio && \
    # Necessary to pass CRI tests: https://github.com/kubernetes-sigs/cri-tools/pull/905
    mkdir -p /etc/crio/crio.conf.d && \
    printf '[crio.runtime]\nseccomp_use_default_when_empty = false\n' > /etc/crio/crio.conf.d/02-seccomp.conf

COPY --from=stargz-store-dev /out/* /usr/local/bin/
COPY --from=cri-o-dev /out/bin/* /usr/local/bin/
COPY --from=cri-o-dev /out/crio.service /etc/systemd/system/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/
COPY --from=conmon-dev /out/bin/* /usr/local/bin/
COPY --from=containers-common-dev /out/seccomp.json /usr/share/containers/
COPY ./script/config-cri-o/ /

ENTRYPOINT [ "/usr/local/bin/entrypoint" ]

# Image which can be used as a node image for KinD
FROM kindest/node:v1.27.3
COPY --from=containerd-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=snapshotter-dev /out/* /usr/local/bin/
COPY ./script/config/ /
RUN apt-get update -y && apt-get install --no-install-recommends -y fuse3 && \
    systemctl enable stargz-snapshotter
ENTRYPOINT [ "/usr/local/bin/kind-entrypoint.sh", "/usr/local/bin/entrypoint", "/sbin/init" ]
