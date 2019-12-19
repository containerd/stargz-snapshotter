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
PLUGIN_DESTDIR ?= /opt/rsnapshotd
CMD_DESTDIR ?= /usr/local
DOCKER_ARGS ?=
GO111MODULE_VALUE=off
PREFIX ?= out/

PLUGINS=stargzfs-linux-amd64.so
CMD=rsnapshotd

PLUGIN_BINARIES=$(addprefix $(PREFIX),$(PLUGINS))
CMD_BINARIES=$(addprefix $(PREFIX),$(CMD))

.PHONY: check build

all: build

build: $(PLUGINS) $(CMD)

FORCE:

stargzfs-linux-amd64.so: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -buildmode=plugin -o $(PREFIX)$@ -v ./filesystems/stargz

rsnapshotd: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ -v ./cmd/rsnapshotd

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
	@mkdir -p $(PLUGIN_DESTDIR)/plugins
	@mkdir -p $(CMD_DESTDIR)/bin
	@install $(PLUGIN_BINARIES) $(PLUGIN_DESTDIR)/plugins
	@install $(CMD_BINARIES) $(CMD_DESTDIR)/bin

uninstall:
	@echo "$@"
	@rm -f $(addprefix $(PLUGIN_DESTDIR)/plugins/,$(notdir $(PLUGIN_BINARIES)))
	@rm -f $(addprefix $(CMD_DESTDIR)/bin/,$(notdir $(CMD_BINARIES)))

clean:
	@echo "$@"
	@rm -f $(PLUGIN_BINARIES)
	@rm -f $(CMD_BINARIES)

test:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test ./...

test-root:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test ./snapshot -test.root

test-all: test-root test

integration:
	@./script/make.sh integration $(DOCKER_ARGS)
