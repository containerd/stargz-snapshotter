FROM ubuntu:18.04

# basic packages.
RUN apt-get update && \
    apt-get install -y btrfs-tools libseccomp-dev pkg-config unzip fuse curl wget git
RUN curl -o go1.12.7.linux-amd64.tar.gz https://dl.google.com/go/go1.12.7.linux-amd64.tar.gz && \
    tar -xf go1.12.7.linux-amd64.tar.gz && \
    mv go /usr/local/ && \
    mkdir /go
ENV GOROOT=/usr/local/go
ENV GOPATH=/go
ENV GO111MODULE=off
ENV PATH=${GOPATH}/bin:${GOROOT}/bin:$PATH

# remote snapshotter related packages.
RUN go get -u "github.com/containerd/continuity/fs" && \
    go get -u "github.com/hanwen/go-fuse/fuse" && \
    go get -u "github.com/pkg/errors" && \
    go get -u "golang.org/x/sys/unix"

# get stargzify command to convert images to stargz format
RUN git clone https://github.com/google/crfs.git $GOPATH/src/github.com/google/crfs && \
    cd $GOPATH/src/github.com/google/crfs && \
    git checkout aea36382db7e217770fad450b6073b1fa16e0eaf && \
    cd stargz/stargzify && GO111MODULE=on go install

# patched version of containerd(with remote-snapshotter support).
RUN git clone https://github.com/opencontainers/runc $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    git checkout d736ef14f0288d6993a1845745d6756cfc9ddd5a && \
    make BUILDTAGS='seccomp apparmor' && make install && \
    git clone https://github.com/ktock/containerd $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    git checkout remote-snapshotter && \
    git checkout 53110e516e5b4c0cd5b81f61c01fd5e33a25edef && \
    make && make install

# helper for building and entrypoint
COPY ./run.sh /
RUN chmod 755 /run.sh &&  mkdir /etc/containerd
COPY ./config.toml /etc/containerd/config.toml

ENTRYPOINT ["/bin/bash"]