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

FROM golang:1.14

# basic packages
RUN apt-get update -y && \
    apt-get install -y libbtrfs-dev libseccomp-dev fuse iptables && \
    update-alternatives --set iptables /usr/sbin/iptables-legacy

# runtime dependencies
RUN git clone https://github.com/opencontainers/runc \
              $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    git checkout d736ef14f0288d6993a1845745d6756cfc9ddd5a && \
    GO111MODULE=off make BUILDTAGS='seccomp apparmor' && \
    GO111MODULE=off make install && \
    git clone https://github.com/containerd/containerd \
              $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    git checkout d5714702d1f78ad1149e78e204db0b53611ddb3c && \
    GO111MODULE=off make && GO111MODULE=off make install && \
    git clone https://github.com/ktock/buildkit \
              $GOPATH/src/github.com/moby/buildkit && \
    cd $GOPATH/src/github.com/moby/buildkit && \
    git checkout f1ecc7824ebcd147a9bd9426d19999a7eb8e9bab && \
    mkdir -p /etc/buildkit /opt/cni/bin && \
    cp ./hack/fixtures/cni.json /etc/buildkit/cni.json && \
    curl -Ls https://github.com/containernetworking/plugins/releases/download/v0.8.5/cni-plugins-linux-amd64-v0.8.5.tgz | tar xzv -C /opt/cni/bin && \
    go build -o /opt/buildkit/master/bin/buildctl ./cmd/buildctl && \
    go build -ldflags "-w -extldflags -static" -tags "osusergo netgo static_build seccomp" \
             -o /opt/buildkit/master/bin/buildkitd ./cmd/buildkitd && \
    git checkout 636d300fd3d07b1458fc93948caf637105620026 && \
    go build -o /opt/buildkit/stargz/bin/buildctl ./cmd/buildctl && \
    go build -ldflags "-w -extldflags -static" -tags "osusergo netgo static_build seccomp" \
             -o /opt/buildkit/stargz/bin/buildkitd ./cmd/buildkitd
