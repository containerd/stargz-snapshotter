module github.com/containerd/stargz-snapshotter/ipfs

go 1.16

require (
	github.com/containerd/containerd v1.5.7
	github.com/ipfs/go-cid v0.1.0
	github.com/ipfs/go-ipfs-files v0.0.9
	github.com/ipfs/go-ipfs-http-client v0.1.0
	github.com/ipfs/interface-go-ipfs-core v0.5.2
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.2-0.20210819154149-5ad6f50d6283
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.8.1
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
)

// Temporary fork for avoiding importing patent-protected code: https://github.com/hashicorp/golang-lru/issues/73
replace github.com/hashicorp/golang-lru => github.com/ktock/golang-lru v0.5.5-0.20211029085301-ec551be6f75c
