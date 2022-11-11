[[⬇️ **Download**]](https://github.com/containerd/stargz-snapshotter/releases)
[[📔**Browse images**]](./docs/pre-converted-images.md)
[[☸**Quick Start (Kubernetes)**]](#quick-start-with-kubernetes)
[[🤓**Quick Start (nerdctl)**]](https://github.com/containerd/nerdctl/blob/master/docs/stargz.md)
[[🔆**Install**]](./docs/INSTALL.md)

# Stargz Snapshotter

[![Tests Status](https://github.com/containerd/stargz-snapshotter/workflows/Tests/badge.svg)](https://github.com/containerd/stargz-snapshotter/actions?query=workflow%3ATests+branch%3Amain)
[![Benchmarking](https://github.com/containerd/stargz-snapshotter/workflows/Benchmark/badge.svg)](https://github.com/containerd/stargz-snapshotter/actions?query=workflow%3ABenchmark+branch%3Amain)
[![Nightly](https://github.com/containerd/stargz-snapshotter/workflows/Nightly/badge.svg)](https://github.com/containerd/stargz-snapshotter/actions?query=workflow%3ANightly+branch%3Amain)

Read also introductory blog: [Startup Containers in Lightning Speed with Lazy Image Distribution on Containerd](https://medium.com/nttlabs/startup-containers-in-lightning-speed-with-lazy-image-distribution-on-containerd-243d94522361)

Pulling image is one of the time-consuming steps in the container lifecycle.
Research shows that time to take for pull operation accounts for 76% of container startup time[[FAST '16]](https://www.usenix.org/node/194431).
*Stargz Snapshotter* is an implementation of snapshotter which aims to solve this problem by *lazy pulling*.
*Lazy pulling* here means a container can run without waiting for the pull completion of the image and necessary chunks of the image are fetched *on-demand*.

[*eStargz*](/docs/stargz-estargz.md) is a lazily-pullable image format proposed by this project.
This is compatible to [OCI](https://github.com/opencontainers/image-spec/)/[Docker](https://github.com/moby/moby/blob/master/image/spec/v1.2.md) images so this can be pushed to standard container registries (e.g. ghcr.io) as well as this is *still runnable* even on eStargz-agnostic runtimes including Docker.
eStargz format is based on [stargz image format by CRFS](https://github.com/google/crfs) but comes with additional features like runtime optimization and content verification.

The following histogram is the benchmarking result for startup time of several containers measured on Github Actions, using GitHub Container Registry.

<img src="docs/images/benchmarking-result-ecdb227.png" width="600" alt="The benchmarking result on ecdb227">

`legacy` shows the startup performance when we use containerd's default snapshotter (`overlayfs`) with images copied from `docker.io/library` without optimization.
For this configuration, containerd pulls entire image contents and `pull` operation takes accordingly.
When we use stargz snapshotter with eStargz-converted images but without any optimization (`estargz-noopt`) we are seeing performance improvement on the `pull` operation because containerd can start the container without waiting for the `pull` completion and fetch necessary chunks of the image on-demand.
But at the same time, we see the performance drawback for `run` operation because each access to files takes extra time for fetching them from the registry.
When we use [eStargz with optimization](/docs/ctr-remote.md) (`estargz`), we can mitigate the performance drawback observed in `estargz-noopt` images.
This is because [stargz snapshotter prefetches and caches *likely accessed files* during running the container](/docs/stargz-estargz.md).
On the first container creation, stargz snapshotter waits for the prefetch completion so `create` sometimes takes longer than other types of image.
But it's still shorter than waiting for downloading all files of all layers.

The above histogram is [the benchmarking result on the commit `ecdb227`](https://github.com/containerd/stargz-snapshotter/actions/runs/398606060).
We are constantly measuring the performance of this snapshotter so you can get the latest one through the badge shown top of this doc.
Please note that we sometimes see dispersion among the results because of the NW condition on the internet and the location of the instance in the Github Actions, etc.
Our benchmarking method is based on [HelloBench](https://github.com/Tintri/hello-bench).

:nerd_face: You can also run containers on IPFS with lazy pulling. This is an experimental feature. See [`./docs/ipfs.md`](./docs/ipfs.md) for more details.

Stargz Snapshotter is a **non-core** sub-project of containerd.

## Quick Start with Kubernetes

- For more details about stargz snapshotter plugin and its configuration, refer to [Containerd Stargz Snapshotter Plugin Overview](/docs/overview.md).
- For more details about setup lazy pulling of eStargz with containerd, CRI-O, Podman, systemd, etc., refer to [Install Stargz Snapshotter and Stargz Store](./docs/INSTALL.md).
- For more details about integration status of eStargz with tools in commuinty, refer to [Integration of eStargz with other tools](./docs/integration.md)

For using stargz snapshotter on kubernetes nodes, you need the following configuration to containerd as well as run stargz snapshotter daemon on the node.
We assume that you are using containerd (> v1.4.2) as a CRI runtime.

```toml
version = 2

# Plug stargz snapshotter into containerd
# Containerd recognizes stargz snapshotter through specified socket address.
# The specified address below is the default which stargz snapshotter listen to.
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"

# Use stargz snapshotter through CRI
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "stargz"
  disable_snapshot_annotations = false
```

**Note that `disable_snapshot_annotations = false` is required since containerd > v1.4.2**

You can try our [prebuilt](/Dockerfile) [KinD](https://github.com/kubernetes-sigs/kind) node image that contains the above configuration.

```console
$ kind create cluster --name stargz-demo --image ghcr.io/containerd/stargz-snapshotter:0.12.1-kind
```

:information_source: kind binary v0.16.x or newer is recommended for `ghcr.io/containerd/stargz-snapshotter:0.12.1-kind`.

:information_source: You can get latest node images from [`ghcr.io/containerd/stargz-snapshotter:${VERSION}-kind`](https://github.com/orgs/containerd/packages/container/package/stargz-snapshotter) namespace.

Then you can create eStargz pods on the cluster.
In this example, we create a stargz-converted Node.js pod (`ghcr.io/stargz-containers/node:17.8.0-esgz`) as a demo.

```yaml
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
```

The following command lazily pulls `ghcr.io/stargz-containers/node:17.8.0-esgz` from Github Container Registry and creates the pod so the time to take for it is shorter than the original image `library/node:13.13`.

```console
$ kubectl --context kind-stargz-demo apply -f stargz-pod.yaml && kubectl --context kind-stargz-demo get po nodejs -w
$ kubectl --context kind-stargz-demo port-forward nodejs 8080:80 &
$ curl 127.0.0.1:8080
Hello World!
```

Stargz snapshotter also supports [further configuration](/docs/overview.md) including private registry authentication, mirror registries, etc.

## Getting eStargz images

- For more examples and details about the image converter `ctr-remote`, refer to [Optimize Images with `ctr-remote image optimize`](/docs/ctr-remote.md).
- For more details about eStargz format, refer to [eStargz: Standard-Compatible Extensions to Tar.gz Layers for Lazy Pulling Container Images](/docs/stargz-estargz.md)

For lazy pulling images, you need to prepare eStargz images first.
There are several ways to achieve that.
This section describes some of them.

### Trying pre-built eStargz images

You can try our pre-converted eStargz images on ghcr.io listed in [Trying pre-converted images](/docs/pre-converted-images.md).

### Building eStargz images using BuildKit

BuildKit supports building eStargz image since v0.10.

You can try it using [Docker Buildx](https://docs.docker.com/buildx/working-with-buildx/).
The following command builds an eStargz image and push it to `ghcr.io/ktock/hello:esgz`.
Flags `oci-mediatypes=true,compression=estargz` enable to build eStargz.

```
$ docker buildx build -t ghcr.io/ktock/hello:esgz \
    -o type=registry,oci-mediatypes=true,compression=estargz,force-compression=true \
    /tmp/buildctx/
```

> NOTE1: `force-compression=true` isn't needed if the base image is already eStargz.

> NOTE2: Docker still does not support lazy pulling of eStargz.

eStargz-enaled BuildKit (v0.10) will be [included to Docker v22.XX](https://github.com/moby/moby/blob/v22.06.0-beta.0/vendor.mod#L51) however you can build eStargz images with the prior version using Buildx [driver](https://github.com/docker/buildx/blob/master/docs/reference/buildx_create.md#-set-the-builder-driver-to-use---driver) feature.
You can enable the specific version of BuildKit using [`docker buildx create`](https://docs.docker.com/engine/reference/commandline/buildx_create/) (this example specifies `v0.10.3`).

```
$ docker buildx create --use --name v0.10.3 --driver docker-container --driver-opt image=moby/buildkit:v0.10.3
$ docker buildx inspect --bootstrap v0.10.3
```

### Building eStargz images using Kaniko

[Kaniko](https://github.com/GoogleContainerTools/kaniko) is an image builder runnable in containers and Kubernetes.
Since v1.5.0, it experimentally supports building eStargz.
`GGCR_EXPERIMENT_ESTARGZ=1` is needed.

```console
$ docker run --rm -e GGCR_EXPERIMENT_ESTARGZ=1 \
    -v /tmp/buildctx:/workspace -v ~/.docker/config.json:/kaniko/.docker/config.json:ro \
    gcr.io/kaniko-project/executor:v1.8.1 --destination ghcr.io/ktock/hello:esgz
```

### Building eStargz images using nerdctl

[nerdctl](https://github.com/containerd/nerdctl), Docker-compatible CLI of containerd, supports building eStargz images.

```console
$ nerdctl build -t ghcr.io/ktock/hello:1 /tmp/buildctx
$ nerdctl image convert --estargz --oci ghcr.io/ktock/hello:1 ghcr.io/ktock/hello:esgz
$ nerdctl push ghcr.io/ktock/hello:esgz
```

> NOTE: `--estargz` should be specified in conjunction with `--oci`

Please refer to nerdctl document for details for further information (e.g. lazy pulling): https://github.com/containerd/nerdctl/blob/master/docs/stargz.md

### Creating eStargz images using `ctr-remote`

[`ctr-remote`](/docs/ctr-remote.md) allows converting an image into eStargz with optimizing it.
As shown in the above benchmarking result, on-demand lazy pulling improves the performance of pull but causes runtime performance penalty because reading files induce remotely downloading contents.
For solving this, `ctr-remote` has *workload-based* optimization for images.

For trying the examples described in this section, you can also use the docker-compose-based demo environment.
You can setup this environment as the following commands (put this repo on `${GOPATH}/src/github.com/containerd/stargz-snapshotter`).
*Note that this runs privileged containers on your host.*

```console
$ cd ${GOPATH}/src/github.com/containerd/stargz-snapshotter/script/demo
$ docker-compose build containerd_demo
$ docker-compose up -d
$ docker exec -it containerd_demo /bin/bash
(inside container) # ./script/demo/run.sh
```

Generally, container images are built with purpose and the *workloads* are defined in the Dockerfile with some parameters (e.g. entrypoint, envvars and user).
By default, `ctr-remote` optimizes the performance of reading files that are most likely accessed in the workload defined in the Dockerfile.
[You can also specify the custom workload using options if needed](/docs/ctr-remote.md).

The following example converts the legacy `library/ubuntu:20.04` image into eStargz.
The command also optimizes the image for the workload of executing `ls` on `/bin/bash`.
The thing actually done is it runs the specified workload in a temporary container and profiles all file accesses with marking them as *likely accessed* also during runtime.
The converted image is still **docker-compatible** so you can run it with eStargz-agnostic runtimes (e.g. Docker).

```console
# ctr-remote image pull docker.io/library/ubuntu:20.04
# ctr-remote image optimize --oci --entrypoint='[ "/bin/bash", "-c" ]' --args='[ "ls" ]' docker.io/library/ubuntu:20.04 registry2:5000/ubuntu:20.04
# ctr-remote image push --plain-http registry2:5000/ubuntu:20.04
```

Finally, the following commands clear the local cache then pull the eStargz image lazily.
Stargz snapshotter prefetches files that are most likely accessed in the optimized workload, which hopefully increases the cache hit rate for that workload and mitigates runtime overheads as shown in the benchmarking result shown top of this doc.

```console
# ctr-remote image rm --sync registry2:5000/ubuntu:20.04
# ctr-remote images rpull --plain-http registry2:5000/ubuntu:20.04
fetching sha256:610399d1... application/vnd.oci.image.index.v1+json
fetching sha256:0b4a26b4... application/vnd.oci.image.manifest.v1+json
fetching sha256:8d8d9dbe... application/vnd.oci.image.config.v1+json
# ctr-remote run --rm -t --snapshotter=stargz registry2:5000/ubuntu:20.04 test /bin/bash
root@8eabb871a9bd:/# ls
bin  boot  dev  etc  home  lib  lib32  lib64  libx32  media  mnt  opt  proc  root  run  sbin  srv  sys  tmp  usr  var
```

> NOTE: You can perform lazy pulling from any OCI-compatible registries (e.g. docker.io, ghcr.io, etc) as long as the image is formatted as eStargz.

### Registry-side conversion with `estargz.kontain.me`

You can convert arbitrary images into eStargz on the registry-side, using [`estargz.kontain.me`](https://estargz.kontain.me).
`estargz.kontain.me/[image]` serves eStargz-converted version of an arbitrary public image.

For example, the following Kubernetes manifest performs lazy pulling of eStargz-formatted version of `docker.io/library/nginx:1.21.1` that is converted by `estargz.kontain.me`.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nginx
spec:
  containers:
  - name: nginx
    image: estargz.kontain.me/docker.io/library/nginx:1.21.1
    ports:
    - containerPort: 80
```

> WARNING: Before trying this method, read [caveats from kontain.me](https://github.com/imjasonh/kontain.me#caveats). If you rely on it in production, you should copy the image to your own registry or build eStargz by your own using `ctr-remote` as described in the following.

## Importing Stargz Snapshotter as go module

Currently, Stargz Snapshotter repository contains two Go modules as the following and both of them need to be imported.

- `github.com/containerd/stargz-snapshotter`
- `github.com/containerd/stargz-snapshotter/estargz`

Please make sure you import the both of them and they point to *the same commit version*.

## Project details

Stargz Snapshotter is a containerd **non-core** sub-project, licensed under the [Apache 2.0 license](./LICENSE).
As a containerd non-core sub-project, you will find the:
 * [Project governance](https://github.com/containerd/project/blob/main/GOVERNANCE.md),
 * [Maintainers](./MAINTAINERS),
 * and [Contributing guidelines](https://github.com/containerd/project/blob/main/CONTRIBUTING.md)

information in our [`containerd/project`](https://github.com/containerd/project) repository.
