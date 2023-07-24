module github.com/containerd/stargz-snapshotter

go 1.19

require (
	github.com/containerd/console v1.0.3
	github.com/containerd/containerd v1.7.2
	github.com/containerd/continuity v0.4.1
	github.com/containerd/stargz-snapshotter/estargz v0.14.3
	github.com/docker/cli v24.0.4+incompatible
	github.com/docker/go-metrics v0.0.1
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da
	github.com/hanwen/go-fuse/v2 v2.3.0
	github.com/hashicorp/go-multierror v1.1.1
	github.com/hashicorp/go-retryablehttp v0.7.4
	github.com/klauspost/compress v1.16.7
	github.com/moby/sys/mountinfo v0.6.2
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.0-rc2.0.20221005185240-3a7f492d3f1b
	github.com/opencontainers/runtime-spec v1.1.0-rc.3
	github.com/prometheus/client_golang v1.16.0
	github.com/rs/xid v1.5.0
	github.com/sirupsen/logrus v1.9.3
	golang.org/x/sync v0.3.0
	golang.org/x/sys v0.10.0
	google.golang.org/grpc v1.56.2
	k8s.io/api v0.27.4
	k8s.io/apimachinery v0.27.4
	k8s.io/client-go v0.27.4
	k8s.io/cri-api v0.28.0-alpha.4
)

require (
	github.com/AdaLogics/go-fuzz-headers v0.0.0-20230106234847-43070de90fa1 // indirect
	github.com/AdamKorcz/go-118-fuzz-build v0.0.0-20221215162035-5330a85ea652 // indirect
	github.com/Microsoft/go-winio v0.6.1 // indirect
	github.com/Microsoft/hcsshim v0.10.0-rc.8 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/containerd/cgroups v1.1.0 // indirect
	github.com/containerd/cgroups/v3 v3.0.1 // indirect
	github.com/containerd/fifo v1.1.0 // indirect
	github.com/containerd/go-cni v1.1.9 // indirect
	github.com/containerd/ttrpc v1.2.2 // indirect
	github.com/containerd/typeurl/v2 v2.1.1 // indirect
	github.com/containernetworking/cni v1.1.2 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.2 // indirect
	github.com/cyphar/filepath-securejoin v0.2.3 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docker/docker v23.0.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.7.0 // indirect
	github.com/docker/go-events v0.0.0-20190806004212-e31b211e4f1c // indirect
	github.com/emicklei/go-restful/v3 v3.10.1 // indirect
	github.com/go-logr/logr v1.2.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v0.19.6 // indirect
	github.com/go-openapi/jsonreference v0.20.1 // indirect
	github.com/go-openapi/swag v0.22.3 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/gnostic v0.5.7-v3refs // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/imdario/mergo v0.3.13 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/moby/locker v1.0.1 // indirect
	github.com/moby/sys/sequential v0.5.0 // indirect
	github.com/moby/sys/signal v0.7.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/runc v1.1.5 // indirect
	github.com/opencontainers/selinux v1.11.0 // indirect
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_model v0.3.0 // indirect
	github.com/prometheus/common v0.42.0 // indirect
	github.com/prometheus/procfs v0.10.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/stretchr/testify v1.8.4 // indirect
	github.com/urfave/cli v1.22.14 // indirect
	github.com/vbatts/tar-split v0.11.3 // indirect
	go.etcd.io/bbolt v1.3.7 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/otel v1.14.0 // indirect
	go.opentelemetry.io/otel/trace v1.14.0 // indirect
	golang.org/x/mod v0.9.0 // indirect
	golang.org/x/net v0.9.0 // indirect
	golang.org/x/oauth2 v0.7.0 // indirect
	golang.org/x/term v0.7.0 // indirect
	golang.org/x/text v0.9.0 // indirect
	golang.org/x/time v0.0.0-20220210224613-90d013bbcef8 // indirect
	golang.org/x/tools v0.7.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/genproto v0.0.0-20230410155749-daa745c078e1 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gotest.tools/v3 v3.4.0 // indirect
	k8s.io/klog/v2 v2.90.1 // indirect
	k8s.io/kube-openapi v0.0.0-20230501164219-8b0f38b5fd1f // indirect
	k8s.io/utils v0.0.0-20230220204549-a5ecb0141aa5 // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/structured-merge-diff/v4 v4.2.3 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
)

// Import local package for estargz.
replace github.com/containerd/stargz-snapshotter/estargz => ./estargz
