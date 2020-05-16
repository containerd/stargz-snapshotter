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

FROM alpine:3.9 AS stage-build

WORKDIR /work
RUN apk add git build-base linux-headers && \
    git clone git://git.kernel.dk/fio.git /work/fio && \
    cd /work/fio && \
    git checkout c96b385b6e0c78478697713e6da9174fba2432d3 && \
    ./configure --prefix=/work/root && \
    make && \
    make install

FROM alpine:3.9 AS stage-final

# How many bytes fio is going to read/write, 512MiB by default.
ARG size=256M

# How many concurrent reading threads.
ARG thread=4

WORKDIR /work
COPY --from=stage-build /work/root/ /

# Lay out test files here, so that they can be compressed into the image layer.
RUN fio -directory=/work -direct=1 -numjobs=$thread -size=$size -name=test -rw=read