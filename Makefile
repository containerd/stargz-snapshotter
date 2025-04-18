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
CMD_DESTDIR ?= /usr/local
GO111MODULE_VALUE=auto
PREFIX ?= $(CURDIR)/out/

PKG=github.com/containerd/stargz-snapshotter
VERSION=$(shell git describe --match 'v[0-9]*' --dirty='.m' --always --tags)
REVISION=$(shell git rev-parse HEAD)$(shell if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi)
GO_BUILD_LDFLAGS ?= -s -w
GO_LD_FLAGS=-ldflags '$(GO_BUILD_LDFLAGS) -X $(PKG)/version.Version=$(VERSION) -X $(PKG)/version.Revision=$(REVISION) $(GO_EXTRA_LDFLAGS)'

CMD=containerd-stargz-grpc ctr-remote stargz-store stargz-fuse-manager

CMD_BINARIES=$(addprefix $(PREFIX),$(CMD))

.PHONY: all build check install uninstall clean test test-root test-all integration test-optimize benchmark test-kind test-cri-containerd test-cri-o test-criauth generate validate-generated test-k3s test-k3s-argo-workflow vendor

all: build

build: $(CMD)

FORCE:

containerd-stargz-grpc: FORCE
	cd cmd/ ; GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./containerd-stargz-grpc

ctr-remote: FORCE
	cd cmd/ ; GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./ctr-remote

stargz-store: FORCE
	cd cmd/ ; GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./stargz-store

stargz-store-helper: FORCE
	cd cmd/ ; GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./stargz-store/helper

stargz-fuse-manager: FORCE
	cd cmd/ ; GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./stargz-fuse-manager

check:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) $(shell go env GOPATH)/bin/golangci-lint run
	@cd ./estargz ; GO111MODULE=$(GO111MODULE_VALUE) $(shell go env GOPATH)/bin/golangci-lint run
	@cd ./cmd ; GO111MODULE=$(GO111MODULE_VALUE) $(shell go env GOPATH)/bin/golangci-lint run
	@cd ./ipfs ; GO111MODULE=$(GO111MODULE_VALUE) $(shell go env GOPATH)/bin/golangci-lint run

install:
	@echo "$@"
	@mkdir -p $(CMD_DESTDIR)/bin
	@install $(CMD_BINARIES) $(CMD_DESTDIR)/bin

uninstall:
	@echo "$@"
	@rm -f $(addprefix $(CMD_DESTDIR)/bin/,$(notdir $(CMD_BINARIES)))

clean:
	@echo "$@"
	@rm -f $(CMD_BINARIES)

generate:
	@./script/generated-files/generate.sh update

validate-generated:
	@./script/generated-files/generate.sh validate

vendor:
	@cd ./estargz ; GO111MODULE=$(GO111MODULE_VALUE) go mod tidy
	@cd ./ipfs ; GO111MODULE=$(GO111MODULE_VALUE) go mod tidy
	@GO111MODULE=$(GO111MODULE_VALUE) go mod tidy
	@cd ./cmd ; GO111MODULE=$(GO111MODULE_VALUE) go mod tidy

test:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test -race ./...
	@cd ./estargz ; GO111MODULE=$(GO111MODULE_VALUE) go test -timeout 30m -race ./...
	@cd ./cmd ; GO111MODULE=$(GO111MODULE_VALUE) go test -timeout 20m -race ./...
	@cd ./ipfs ; GO111MODULE=$(GO111MODULE_VALUE) go test -timeout 20m -race ./...

test-root:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test -race ./snapshot -test.root

test-all: test-root test

integration:
	@./script/integration/test.sh

test-optimize:
	@./script/optimize/test.sh

benchmark:
	@./script/benchmark/test.sh

test-kind:
	@./script/kind/test.sh

test-cri-containerd:
	@./script/cri-containerd/test.sh

test-cri-o:
	@./script/cri-o/test.sh

test-podman:
	@./script/podman/test.sh

test-criauth:
	@./script/criauth/test.sh

test-k3s:
	@./script/k3s/test.sh

test-k3s-argo-workflow:
	@./script/k3s-argo-workflow/run.sh

test-ipfs:
	@./script/ipfs/test.sh

validate-vendor:
	$(eval TMPDIR := $(shell mktemp -d))
	@cp -R $(CURDIR) ${TMPDIR}
	@(cd ${TMPDIR}/stargz-snapshotter && make vendor)
	@diff -r -u -q $(CURDIR) ${TMPDIR}/stargz-snapshotter
	@rm -rf ${TMPDIR}
