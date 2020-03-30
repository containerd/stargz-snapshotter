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

FROM golang:1.13

# basic tools
RUN apt-get update -y && \
    apt-get install --no-install-recommends -y libbtrfs-dev libseccomp-dev fuse python \
            apt-transport-https software-properties-common && \
    curl -fsSL https://download.docker.com/linux/debian/gpg | apt-key add - && \
    add-apt-repository \
      "deb [arch=amd64] https://download.docker.com/linux/debian $(lsb_release -cs) stable" && \
    apt-get update -y && apt-get install --no-install-recommends -y docker-ce-cli

# runtime dependencies
RUN git clone https://github.com/google/go-containerregistry \
              $GOPATH/src/github.com/google/go-containerregistry && \
    cd $GOPATH/src/github.com/google/go-containerregistry && \
    git checkout 4b1985e5ea2104672636879e1694808f735fd214 && \
    GO111MODULE=on go install github.com/google/go-containerregistry/cmd/crane && \
    git clone https://github.com/opencontainers/runc \
              $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    git checkout d736ef14f0288d6993a1845745d6756cfc9ddd5a && \
    GO111MODULE=off make -j2 BUILDTAGS='seccomp apparmor' && \
    GO111MODULE=off make install && \
    git clone https://github.com/containerd/containerd \
              $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    git checkout d5714702d1f78ad1149e78e204db0b53611ddb3c && \
    GO111MODULE=off make -j2 && GO111MODULE=off make install
