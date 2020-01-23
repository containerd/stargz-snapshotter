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

FROM golang:1.12

# basic packages
# docker-ce-cli is used only for users to log into registries (e.g. DockerHub)
# with ~/.docker/config.json
RUN apt-get update -y && \
    apt-get install -y libbtrfs-dev libseccomp-dev fuse \
                       apt-transport-https gnupg2 software-properties-common && \
    curl -fsSL https://download.docker.com/linux/debian/gpg | apt-key add - && \
    add-apt-repository \
      "deb [arch=amd64] https://download.docker.com/linux/debian $(lsb_release -cs) stable" && \
    apt-get update -y && apt-get install -y docker-ce-cli

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
    GO111MODULE=off make && GO111MODULE=off make install
