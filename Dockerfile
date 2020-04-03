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

FROM golang:1.13-buster AS golang-base

# build customized version of containerd which supports remote snapshots via CRI
FROM golang-base AS containerd-builder
RUN apt-get update -y && \
    apt-get install -y libbtrfs-dev libseccomp-dev && \
    git clone https://github.com/ktock/containerd \
              $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    git checkout 7b2ddf1589694a52f5df5ff351bd2764fb1d0a3a && \
    GO111MODULE=off make && DESTDIR=/out/ make install

# build stargz snapshotter
FROM golang-base AS snapshotter-builder
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN cd $GOPATH/src/github.com/containerd/stargz-snapshotter && PREFIX=/out/ make -j4

FROM kindest/node:v1.18.0

# install containerd and snapshotter plugin from building stages
COPY --from=containerd-builder /out/bin/* /usr/local/bin/
COPY --from=snapshotter-builder /out/* /usr/local/bin/
COPY ./script/pullsecrets/config/stargz-snapshotter.service /etc/systemd/system/
COPY ./script/pullsecrets/config/config.stargz.toml /etc/containerd-stargz-grpc/config.toml
COPY ./script/pullsecrets/config/config.containerd.toml /etc/containerd/config.toml
RUN apt-get update -y && apt-get install --no-install-recommends -y fuse && \
    systemctl enable stargz-snapshotter

ENTRYPOINT [ "/usr/local/bin/entrypoint", "/sbin/init" ]
