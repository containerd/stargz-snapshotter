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


# Base path used to install.
DESTDIR ?= /opt/containerd
DOCKER_ARGS ?=
GO111MODULE_VALUE=off

PLUGINS=remotesn-linux-amd64.so stargzfs-linux-amd64.so

BINARIES=$(addprefix plugins/,$(PLUGINS))

.PHONY: check build

all: build

build: $(BINARIES)

FORCE:

plugins/remotesn-linux-amd64.so: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -buildmode=plugin -o $@ -v .

plugins/stargzfs-linux-amd64.so: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -buildmode=plugin -o $@ -v ./filesystems/stargz

# TODO: git-validation
check:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) golangci-lint run
	@$(GOPATH)/src/github.com/containerd/project/script/validate/fileheader $(GOPATH)/src/github.com/containerd/project/

install-check-tools:
	@curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s -- -b $(go env GOPATH)/bin v1.19.1
	@go get -u github.com/vbatts/git-validation
	@go get -u github.com/kunalkushwaha/ltag
	@git clone https://github.com/containerd/project $(GOPATH)/src/github.com/containerd/project

install:
	@echo "$@"
	@mkdir -p $(DESTDIR)/plugins
	@install $(BINARIES) $(DESTDIR)/plugins

uninstall:
	@echo "$@"
	@rm -f $(addprefix $(DESTDIR)/plugins/,$(notdir $(BINARIES)))

clean:
	@echo "$@"
	@rm -f $(BINARIES)

test:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test ./...

test-root:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test -test.root

test-all: test-root test

integration:
	@./script/make_wrapper/make.sh integration $(DOCKER_ARGS)
