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
    go get -u "github.com/opencontainers/runc" && \
    go get -u "github.com/pkg/errors" && \
    go get -u "golang.org/x/sys/unix"

# patched version of stargzify(forcing stargzify to use HTTP for pushing).
RUN mkdir -p $GOPATH/src/github.com/google && \
    cd $GOPATH/src/github.com/google && \
    git clone https://github.com/google/crfs.git && \
    cd crfs/stargz/stargzify && \
    sed -i -e "s/name.ParseReference(dst)/name.ParseReference(dst, name.Insecure)/g" \
           stargzify.go && \
    GO111MODULE=on go install

# patched version of containerd(with remote-snapshotter support).
RUN mkdir -p $GOPATH/src/github.com/containerd && \
    cd $GOPATH/src/github.com/containerd && \
    git clone https://github.com/ktock/containerd && \
    cd containerd && \
    git checkout remote-snapshotter

# helper for building and entrypoint
COPY ./build.sh ./entrypoint.sh /
RUN chmod 755 /build.sh && \
    chmod 755 /entrypoint.sh && \
    mkdir /etc/containerd
COPY ./config.toml /etc/containerd/config.toml

ENTRYPOINT ["/entrypoint.sh"]