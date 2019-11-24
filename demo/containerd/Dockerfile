FROM ubuntu:18.04

# basic packages.
RUN apt-get update && \
    apt-get install -y btrfs-tools \
                       libseccomp-dev \
                       pkg-config \
                       unzip \
                       fuse \
                       curl \
                       wget \
                       git \
                       apt-transport-https \
                       ca-certificates \
                       curl \
                       gnupg-agent \
                       software-properties-common && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | apt-key add - && \
    apt-key fingerprint 0EBFCD88 && \
    add-apt-repository "deb [arch=amd64] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" && \
    apt-get update -y && apt-get install -y docker-ce-cli

# golang
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
    go get -u "github.com/golang/groupcache/lru" && \
    go get -u "github.com/pkg/errors" && \
    go get -u "golang.org/x/sys/unix" && \
    git clone https://github.com/google/go-containerregistry \
              $GOPATH/src/github.com/google/go-containerregistry && \
    cd $GOPATH/src/github.com/google/go-containerregistry && \
    git checkout b77a90c667afa5efd3fa108f4c2381c54ecf9a69

# get stargzify command to convert images to stargz format
RUN git clone https://github.com/google/crfs.git $GOPATH/src/github.com/google/crfs && \
    cd $GOPATH/src/github.com/google/crfs && \
    git checkout 12599487789fa1d5abead897932c40a1a17929dd && \
    cd stargz/stargzify && GO111MODULE=on go install

# patched version of containerd(with remote-snapshotter support).
RUN git clone https://github.com/opencontainers/runc $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    git checkout d736ef14f0288d6993a1845745d6756cfc9ddd5a && \
    make BUILDTAGS='seccomp apparmor' && make install && \
    git clone https://github.com/ktock/containerd $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    git checkout filter-by-snapshotter-test && \
    git checkout 516ed248600081ea9bf12f4c93b3e491c78859cf && \
    make && make install

# helper for building and entrypoint
COPY ./run.sh ./config.sh ./test.sh /
RUN chmod 755 /run.sh /config.sh /test.sh && mkdir /etc/containerd
COPY ./config.toml /etc/containerd/config.toml

ENTRYPOINT ["/bin/bash"]