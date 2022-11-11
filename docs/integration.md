# Integration of eStargz with other tools

This document lists links and information about integrations of stargz-snapshotter with tools in commuinty.

You can refer to [issue #258 "Tracker issue for adoption status"](https://github.com/containerd/stargz-snapshotter/issues/258) for the list of the latest status of these integrations.

## Kubernetes

To use stargz snapshotter on Kubernetes nodes, you need to use containerd as the CRI runtime.
You also need to run stargz snapshotter on the node.

### Kind

See [`/README.md#quick-start-with-kubernetes`](/README.md#quick-start-with-kubernetes).

### k3s

k3s >= v1.22 supports stagz-snapshotter as an experimental feature.
`--snapshotter=stargz` for k3s server and agent enables this feature.

```
k3s server --snapshotter=stargz
```

Refer to [k3s docs](https://docs.k3s.io/advanced#enabling-lazy-pulling-of-estargz-experimental) for more details.

The following is a quick demo using [k3d](https://github.com/k3d-io/k3d) (k3s in Docker).

```console
$ k3d cluster create mycluster --k3s-arg='--snapshotter=stargz@server:*;agent:*'
$ cat <<'EOF' | kubectl --context=k3d-mycluster apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: nodejs
spec:
  containers:
  - name: nodejs-stargz
    image: ghcr.io/stargz-containers/node:17.8.0-esgz
    command: ["node"]
    args:
    - -e
    - var http = require('http');
      http.createServer(function(req, res) {
        res.writeHead(200);
        res.end('Hello World!\n');
      }).listen(80);
    ports:
    - containerPort: 80
EOF
$ kubectl --context=k3d-mycluster get po nodejs -w
$ kubectl --context=k3d-mycluster port-forward nodejs 8080:80 &
$ curl 127.0.0.1:8080
Hello World!
$ k3d cluster delete mycluster
```

### Google Kubernetes Engine

There is no node image includes stargz snapshotter by default as of now so you need to manually customize the nodes.

A brief instrcution of enabling stargz snapshotter is the following:

- Create a Kubernetes cluster using containerd-supported Linux node images like `ubuntu_containerd`. containerd must be >= v1.4.2.
- SSH into each node and install stargz snapshotter following [`./INSTALL.md`](./INSTALL.md#install-stargz-snapshotter-for-containerd-with-systemd). You need this installation on all worker nodes.
- Optionally apply configuration to allow stargz-snapshotter to access private registries following [`./overview.md`](./overview.md#authentication).

### Amazon Elastic Kubernetes Service

There is no AMI includes stargz snapshotter by default as of now so you need to manually customize the nodes.

A brief instrcution of enabling stargz snapshotter is the following:

- Create a Kubernetes cluster using containerd-supported Linux AMIs. containerd must be >= v1.4.2. e.g. Amazon EKS optimized Amazon Linux AMIs with [containerd runtime bootstrap flag](https://docs.aws.amazon.com/eks/latest/userguide/eks-optimized-ami.html).
- SSH into each node and install stargz snapshotter following [`./INSTALL.md`](./INSTALL.md#install-stargz-snapshotter-for-containerd-with-systemd). You need this installation on all worker nodes.
- Optionally apply configuration to allow stargz-snapshotter to access private registries following [`./overview.md`](./overview.md#authentication).

## CRI runtimes

### containerd

See [`./INSTALL.md`](./INSTALL.md#install-stargz-snapshotter-for-containerd-with-systemd)

> :information_source: There is also a doc for [integration with firecracker-containerd](https://github.com/firecracker-microvm/firecracker-containerd/blob/24f1fcf99ebf6edcb94edd71a2affbcdae6b08e7/docs/remote-snapshotter-getting-started.md).

### CRI-O

See [`./INSTALL.md`](./INSTALL.md#install-stargz-store-for-cri-opodman-with-systemd).

## High-level container engines

### Docker

Docker Desktop 4.12.0 "Containerd Image Store (Beta)" uses stargz-snapshotter.
Refer to [Docker documentation](https://docs.docker.com/desktop/containerd/).

### nerdctl

See the [docs in nerdctl](https://github.com/containerd/nerdctl/blob/main/docs/stargz.md).

### Podman

See [`./INSTALL.md`](./INSTALL.md#install-stargz-store-for-cri-opodman-with-systemd).

## Image builders

### BuildKit

#### Building eStargz

BuildKit >= v0.10 supports creating eStargz images.
See [`README.md`](/README.md#building-estargz-images-using-buildkit) for details.

#### Lazy pulling of eStargz

BuildKit >= v0.8 supports stargz-snapshotter and can perform lazy pulling of eStargz-formatted base images during build.
`--oci-worker-snapshotter=stargz` flag enables this feature.

You can try this feature using Docker Buildx as the following.

```
$ docker buildx create --use --name lazy-builder --buildkitd-flags '--oci-worker-snapshotter=stargz'
$ docker buildx inspect --bootstrap lazy-builder
```

The following is a sample Dockerfile that uses eStargz-formatted golang image (`ghcr.io/stargz-containers/golang:1.18-esgz`) as the base image.

```Dockerfile
FROM ghcr.io/stargz-containers/golang:1.18-esgz AS dev
COPY ./hello.go /hello.go
RUN go build -o /hello /hello.go

FROM scratch
COPY --from=dev /hello /
ENTRYPOINT [ "/hello" ]
```

Put the following Go source code in the context directory with naming it `hello.go`.

```golang
package main

import "fmt"

func main() {
	fmt.Println("Hello, world!")
}
```

The following build performs lazy pulling of the eStargz-formatted golang base image.

```console
$ docker buildx build --load -t hello /tmp/ctx/
$ docker run --rm hello
Hello, world!
```

### Kaniko

#### Building eStargz

Kaniko >= v1.5.0 creates eStargz images when `GGCR_EXPERIMENT_ESTARGZ=1` is specified.
See [`README.md`](/README.md#building-estargz-images-using-kaniko) for details.

### ko

ko >= v0.7.0 creates eStargz images when `GGCR_EXPERIMENT_ESTARGZ=1` is specified.
Please see also [the docs in ko](https://github.com/ko-build/ko/blob/f70e3cad38c3bbd232f51604d922b8baff31144e/docs/advanced/faq.md#can-i-optimize-images-for-estargz-support).

## P2P image distribution

### IPFS

See [`./ipfs.md`](./ipfs.md)

### Dragonfly

Change the `/etc/containerd-stargz-grpc/config.toml` configuration to make dragonfly as registry mirror.
`127.0.0.1:65001` is the proxy address of dragonfly peer,
and the `X-Dragonfly-Registry` header is the address of origin registry,
which is provided for dragonfly to download the images.

```toml
[[resolver.host."docker.io".mirrors]]
  host = "127.0.0.1:65001"
  insecure = true
  [resolver.host."docker.io".mirrors.header]
    X-Dragonfly-Registry = ["https://index.docker.io"]
```

For more details about dragonfly as registry mirror,
refer to [How to use Dragonfly With eStargz](https://d7y.io/docs/setup/integration/stargz/).

## Registry-side conversion of eStargz

### Harbor

See the docs in Harbor: https://github.com/goharbor/acceleration-service
