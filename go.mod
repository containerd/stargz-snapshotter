module github.com/containerd/stargz-snapshotter

go 1.15

require (
	github.com/BurntSushi/toml v0.3.1
	github.com/containerd/console v1.0.1
	github.com/containerd/containerd v1.5.0-beta.0.0.20210122062454-5a66c2ae5cec
	github.com/containerd/continuity v0.0.0-20201208142359-180525291bb7
	github.com/containerd/go-cni v1.0.1
	github.com/containerd/stargz-snapshotter/estargz v0.4.1
	github.com/containernetworking/plugins v0.8.7 // indirect
	github.com/containers/storage v1.24.4
	github.com/coreos/go-systemd/v22 v22.1.0
	github.com/docker/cli v0.0.0-20191017083524-a8ff7f821017
	github.com/docker/docker v17.12.0-ce-rc1.0.20200730172259-9f28837c1d93+incompatible // indirect
	github.com/golang/groupcache v0.0.0-20200121045136-8c9f03a8e57e
	github.com/google/go-containerregistry v0.4.1
	github.com/hanwen/go-fuse/v2 v2.0.4-0.20201208195215-4a458845028b
	github.com/hashicorp/go-multierror v1.1.0
	github.com/hashicorp/golang-lru v0.5.3 // indirect
	github.com/moby/sys/mountinfo v0.4.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.1
	github.com/opencontainers/runtime-spec v1.0.3-0.20200728170252-4d89ac9fbff6
	github.com/pkg/errors v0.9.1
	github.com/rs/xid v1.2.1
	github.com/sirupsen/logrus v1.7.0
	github.com/urfave/cli v1.22.2
	golang.org/x/sync v0.0.0-20201207232520-09787c993a3a
	golang.org/x/sys v0.0.0-20210119212857-b64e53b001e4
	google.golang.org/grpc v1.35.0
	k8s.io/api v0.20.1
	k8s.io/apimachinery v0.20.1
	k8s.io/client-go v0.20.1
)

replace (
	// Import local package for estargz.
	github.com/containerd/stargz-snapshotter/estargz => ./estargz

	// NOTE: github.com/containerd/containerd v1.4.0 depends on github.com/urfave/cli v1.22.1
	//       because of https://github.com/urfave/cli/issues/1092
	github.com/urfave/cli => github.com/urfave/cli v1.22.1
)
