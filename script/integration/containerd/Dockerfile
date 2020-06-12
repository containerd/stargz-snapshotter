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

FROM stargz/cind:1.4.0-beta.1

# basic tools
RUN apt-get update -y && \
    apt-get --no-install-recommends install -y iptables jq && \
    git clone https://github.com/google/go-containerregistry \
              $GOPATH/src/github.com/google/go-containerregistry && \
    cd $GOPATH/src/github.com/google/go-containerregistry && \
    git checkout eb7c14b719c60883de5747caa25463b44f8bf896 && \
    GO111MODULE=on go install github.com/google/go-containerregistry/cmd/crane

COPY ./config.containerd.toml /etc/containerd/config.toml
COPY ./config.stargz.toml /etc/containerd-stargz-grpc/config.toml
