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

FROM golang:1.15

# basic tools
# docker-ce-cli is used only to log into registry with ~/.docker/config.json
RUN apt-get update -y && \
    apt-get --no-install-recommends install -y fuse apt-transport-https gnupg2 software-properties-common jq iptables && \
    curl -fsSL https://download.docker.com/linux/debian/gpg | apt-key add - && \
    add-apt-repository \
      "deb [arch=amd64] https://download.docker.com/linux/debian $(lsb_release -cs) stable" && \
    apt-get update -y && apt-get --no-install-recommends install -y docker-ce-cli runc && \
    GO111MODULE=on go get github.com/google/go-containerregistry/cmd/crane
