module github.com/containerd/stargz-snapshotter/ipfs

go 1.16

require (
	github.com/containerd/containerd v1.6.1
	github.com/ipfs/go-cid v0.1.0
	github.com/ipfs/go-ipfs-files v0.1.1
	github.com/ipfs/interface-go-ipfs-core v0.6.0
	github.com/libp2p/go-libp2p-record v0.1.1 // indirect
	github.com/opencontainers/image-spec v1.0.2-0.20211117181255-693428a734f5
)

// Temporary fork for avoiding importing patent-protected code: https://github.com/hashicorp/golang-lru/issues/73
replace github.com/hashicorp/golang-lru => github.com/ktock/golang-lru v0.5.5-0.20211029085301-ec551be6f75c
