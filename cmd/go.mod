module github.com/containerd/stargz-snapshotter/cmd

go 1.16

require (
	github.com/containerd/containerd v1.7.0-beta.0.0.20221118211334-792294ce06bf
	github.com/containerd/go-cni v1.1.7
	github.com/containerd/stargz-snapshotter v0.13.0
	github.com/containerd/stargz-snapshotter/estargz v0.13.0
	github.com/containerd/stargz-snapshotter/ipfs v0.13.0
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/docker/go-metrics v0.0.1
	github.com/goccy/go-json v0.10.0
	github.com/hashicorp/go-multierror v1.1.1
	github.com/ipfs/go-ipfs-http-client v0.4.0
	github.com/ipfs/interface-go-ipfs-core v0.7.0
	github.com/klauspost/compress v1.15.12
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.0-rc2.0.20221005185240-3a7f492d3f1b
	github.com/opencontainers/runtime-spec v1.0.3-0.20210326190908-1c3f411f0417
	github.com/pelletier/go-toml v1.9.5
	github.com/rs/xid v1.4.0
	github.com/sirupsen/logrus v1.9.0
	github.com/urfave/cli v1.22.9
	go.etcd.io/bbolt v1.3.6
	golang.org/x/sync v0.1.0
	golang.org/x/sys v0.3.0
	google.golang.org/grpc v1.50.1
	k8s.io/cri-api v0.27.0-alpha.0
)

replace (
	// Import local packages.
	github.com/containerd/stargz-snapshotter => ../
	github.com/containerd/stargz-snapshotter/estargz => ../estargz
	github.com/containerd/stargz-snapshotter/ipfs => ../ipfs

	// Temporary fork for avoiding importing patent-protected code: https://github.com/hashicorp/golang-lru/issues/73
	github.com/hashicorp/golang-lru => github.com/ktock/golang-lru v0.5.5-0.20211029085301-ec551be6f75c
)
