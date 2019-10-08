#!/bin/sh

cd /go/src/github.com/opencontainers/runc
make BUILDTAGS='seccomp apparmor' && make install

cd /go/src/github.com/containerd/containerd
make && make install

go build -buildmode=plugin -o /opt/containerd/plugins/remotesn-linux-amd64.so /go/src/github.com/ktock/remote-snapshotter
go build -buildmode=plugin -o /opt/containerd/plugins/stargzfs-linux-amd64.so /go/src/github.com/ktock/remote-snapshotter/filesystems/stargz
