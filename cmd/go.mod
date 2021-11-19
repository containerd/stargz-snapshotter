module github.com/containerd/stargz-snapshotter/cmd

go 1.16

require (
	github.com/containerd/containerd v1.6.0-beta.2.0.20211117185425-a776a27af54a
	github.com/containerd/containerd/api v1.6.0-beta.2.0.20211117185425-a776a27af54a
	github.com/containerd/go-cni v1.1.1-0.20211026134925-aa8bf14323a5
	github.com/containerd/stargz-snapshotter v0.10.1
	github.com/containerd/stargz-snapshotter/estargz v0.10.1
	github.com/containerd/stargz-snapshotter/ipfs v0.10.1
	github.com/coreos/go-systemd/v22 v22.3.2
	github.com/docker/go-metrics v0.0.1
	github.com/hashicorp/go-multierror v1.1.1
	github.com/ipfs/go-ipfs-http-client v0.1.0
	github.com/ipfs/interface-go-ipfs-core v0.5.2
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.2-0.20210819154149-5ad6f50d6283
	github.com/opencontainers/runtime-spec v1.0.3-0.20210326190908-1c3f411f0417
	github.com/pelletier/go-toml v1.9.4
	github.com/pkg/errors v0.9.1
	github.com/rs/xid v1.3.0
	github.com/sirupsen/logrus v1.8.1
	github.com/urfave/cli v1.22.5
	golang.org/x/sys v0.0.0-20211025201205-69cdffdb9359
	google.golang.org/grpc v1.42.0
	k8s.io/cri-api v0.23.0-alpha.4
)

replace (
	// Import local packages.
	github.com/containerd/stargz-snapshotter => ../
	github.com/containerd/stargz-snapshotter/estargz => ../estargz
	github.com/containerd/stargz-snapshotter/ipfs => ../ipfs

	// Temporary fork for avoiding importing patent-protected code: https://github.com/hashicorp/golang-lru/issues/73
	github.com/hashicorp/golang-lru => github.com/ktock/golang-lru v0.5.5-0.20211029085301-ec551be6f75c
)
