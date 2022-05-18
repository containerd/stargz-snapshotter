module github.com/containerd/stargz-snapshotter/cmd

go 1.16

require (
	github.com/containerd/containerd v1.6.4
	github.com/containerd/go-cni v1.1.5
	github.com/containerd/stargz-snapshotter v0.11.4
	github.com/containerd/stargz-snapshotter/estargz v0.11.4
	github.com/containerd/stargz-snapshotter/ipfs v0.11.4
	github.com/coreos/go-systemd/v22 v22.3.2
	github.com/docker/go-metrics v0.0.1
	github.com/goccy/go-json v0.9.7
	github.com/hashicorp/go-multierror v1.1.1
	github.com/ipfs/go-ipfs-http-client v0.3.1
	github.com/ipfs/interface-go-ipfs-core v0.7.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.3-0.20211202183452-c5a74bcca799
	github.com/opencontainers/runtime-spec v1.0.3-0.20210326190908-1c3f411f0417
	github.com/pelletier/go-toml v1.9.5
	github.com/rs/xid v1.4.0
	github.com/sirupsen/logrus v1.8.1
	github.com/urfave/cli v1.22.5
	go.etcd.io/bbolt v1.3.6
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys v0.0.0-20220405210540-1e041c57c461
	google.golang.org/grpc v1.46.2
	k8s.io/cri-api v0.25.0-alpha.0
)

replace (
	// Import local packages.
	github.com/containerd/stargz-snapshotter => ../
	github.com/containerd/stargz-snapshotter/estargz => ../estargz
	github.com/containerd/stargz-snapshotter/ipfs => ../ipfs

	// Temporary fork for avoiding importing patent-protected code: https://github.com/hashicorp/golang-lru/issues/73
	github.com/hashicorp/golang-lru => github.com/ktock/golang-lru v0.5.5-0.20211029085301-ec551be6f75c
)
