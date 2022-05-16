module github.com/containerd/stargz-snapshotter

go 1.16

require (
	github.com/containerd/console v1.0.3
	github.com/containerd/containerd v1.6.4
	github.com/containerd/continuity v0.3.0
	github.com/containerd/stargz-snapshotter/estargz v0.11.4
	github.com/docker/cli v20.10.16+incompatible
	github.com/docker/docker v20.10.7+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.6.4 // indirect
	github.com/docker/go-metrics v0.0.1
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da
	github.com/hanwen/go-fuse/v2 v2.1.1-0.20220112183258-f57e95bda82d
	github.com/hashicorp/go-multierror v1.1.1
	github.com/hashicorp/go-retryablehttp v0.7.1
	github.com/klauspost/compress v1.15.4
	github.com/moby/sys/mountinfo v0.6.1
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.3-0.20211202183452-c5a74bcca799
	github.com/opencontainers/runtime-spec v1.0.3-0.20210326190908-1c3f411f0417
	github.com/pelletier/go-toml v1.9.4 // indirect
	github.com/prometheus/client_golang v1.12.1
	github.com/rs/xid v1.4.0
	github.com/sirupsen/logrus v1.8.1
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys v0.0.0-20220405210540-1e041c57c461
	google.golang.org/grpc v1.46.2
	k8s.io/api v0.24.0
	k8s.io/apimachinery v0.24.0
	k8s.io/client-go v0.24.0
	k8s.io/cri-api v0.25.0-alpha.0
)

replace (
	// Import local package for estargz.
	github.com/containerd/stargz-snapshotter/estargz => ./estargz

	// Temporary fork for avoiding importing patent-protected code: https://github.com/hashicorp/golang-lru/issues/73
	github.com/hashicorp/golang-lru => github.com/ktock/golang-lru v0.5.5-0.20211029085301-ec551be6f75c

	// NOTE1: github.com/containerd/containerd v1.4.0 depends on github.com/urfave/cli v1.22.1
	//        because of https://github.com/urfave/cli/issues/1092
	// NOTE2: Automatic upgrade of this is disabled in denendabot.yml. When we remove this replace
	//        directive, we must remove the corresponding "ignore" configuration from dependabot.yml
	github.com/urfave/cli => github.com/urfave/cli v1.22.1
)
