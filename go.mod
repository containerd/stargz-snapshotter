module github.com/containerd/stargz-snapshotter

go 1.13

require (
	github.com/BurntSushi/toml v0.3.1
	github.com/Microsoft/hcsshim v0.8.9 // indirect
	github.com/Microsoft/hcsshim/test v0.0.0-20200826032352-301c83a30e7c // indirect
	github.com/containerd/cgroups v0.0.0-20200710171044-318312a37340 // indirect
	github.com/containerd/containerd v1.4.0
	github.com/containerd/continuity v0.0.0-20200710164510-efbc4488d8fe
	github.com/containerd/fifo v0.0.0-20200410184934-f15a3290365b // indirect
	github.com/containerd/go-runc v0.0.0-20200220073739-7016d3ce2328
	github.com/containerd/ttrpc v1.0.1 // indirect
	github.com/containerd/typeurl v1.0.1 // indirect
	github.com/docker/cli v0.0.0-20191017083524-a8ff7f821017
	github.com/docker/docker v17.12.0-ce-rc1.0.20200730172259-9f28837c1d93+incompatible
	github.com/gogo/googleapis v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20200121045136-8c9f03a8e57e
	github.com/google/crfs v0.0.0-20191108021818-71d77da419c9
	github.com/google/go-containerregistry v0.1.2
	github.com/hanwen/go-fuse/v2 v2.0.3
	github.com/hashicorp/go-multierror v1.0.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.1
	github.com/opencontainers/runc v1.0.0-rc92
	github.com/opencontainers/runtime-spec v1.0.3-0.20200728170252-4d89ac9fbff6
	github.com/pkg/errors v0.9.1
	github.com/rs/xid v1.2.1
	github.com/sirupsen/logrus v1.6.0
	github.com/urfave/cli v1.22.2
	golang.org/x/sync v0.0.0-20200625203802-6e8e738ad208
	golang.org/x/sys v0.0.0-20200728102440-3e129f6d46b1
	google.golang.org/grpc v1.29.1
	gotest.tools/v3 v3.0.2 // indirect
	k8s.io/api v0.19.0
	k8s.io/apimachinery v0.19.0
	k8s.io/client-go v0.19.0
)

// NOTE: github.com/containerd/containerd v1.4.0 depends on github.com/urfave/cli v1.22.1
//       because of https://github.com/urfave/cli/issues/1092
replace github.com/urfave/cli => github.com/urfave/cli v1.22.1
