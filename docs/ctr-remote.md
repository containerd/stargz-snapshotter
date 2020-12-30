# Optimize Images with `ctr-remote image optimize`

This doc describes example usages of `ctr-remote image optimize` command for converting images into eStargz.

`ctr-remote images optimize` command (call `ctr-remote` in this doc) enables users to convert an image into eStargz.
The converted image can be lazily pulled by Stargz Snapshotter which can speed up the container startup.
Because this image is backward compatible to OCI/Docker image, this can be also run by other runtimes that don't support lazy pull (e.g. Docker).

Though lazy pull speeds up the container's startup, it's possible, especially with slow network, that the runtime performance becomes lower because reading files can induce remotely downloading file contents.
For mitigating this, `ctr-remote` also allows to *optimize* the image against the *workload* the image runs.
Here, workload means the configuration of the container that runs from that image, including the entrypoint program, environment variables, user etc.
This optimization is done by baking the information about files that are likely accessed during runtime (called *prioritized files*), to the image.
On runtime, Stargz Snapshotter prefetches these prioritized files before mounting the layer for making sure these files are locally accessible.
This can avoid downloading chunks on every file read and mitigate the runtime performance drawbacks.

For more details about eStargz and its optimization, refer also to [eStargz: Standard-Compatible Extensions to Tar.gz Layers for Lazy Pulling Container Images](/docs/stargz-estargz.md).

## Requirements

- fusermount: Installable through `fuse` (or `fuse3`) using package manager. e.g. `apt install fuse` on ubuntu.
- runc: Release binaries are available on https://github.com/opencontainers/runc/. Or you should already have this if you are using container runtimes (containerd, Docker, etc.) on your host.
- CNI plugins (if network connection is needed during optimization): Release binaries are available on https://github.com/containernetworking/plugins.

`ctr-remote` requires the root privilege, so run this command by root or with `sudo`.
Rootless execution of this command is still WIP.

For trying the examples described in this doc, you can also use the docker-compose-based demo environment.
You can setup this environment as the following commands.
*Note that this runs privileged containers on your host.*

```
$ cd ${GOPATH}/src/github.com/containerd/stargz-snapshotter/script/demo
$ docker-compose build containerd_demo
$ docker-compose up -d
$ docker exec -it containerd_demo /bin/bash
(inside container) # ./script/demo/run.sh
```

## Optimizing an image

The following command optimizes an (non-eStargz) image `ghcr.io/stargz-containers/golang:1.15.3-buster-org` (this is a copy of `golang:1.15.3-buster`) and pushes the result eStargz image into `registry2:5000/golang:1.15.3-esgz`.
This doesn't append workload-related configuration options (e.g. `--entrypoint`) so this optimizes the image against the default configurations baked to the image e.g. through Dockefile instructions (`ENTRYPOINT`, etc) when building the original image.

Here, `registry2:5000` in the demo environment serves images with HTTP (not HTTPS) so we need to tell it to the `ctr-remote` command using `--plain-http` option and `http://` prefix.

```
ctr-remote image optimize --plain-http \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           http://registry2:5000/golang:1.15.3-esgz
```

When you run this command, `ctr-remote` runs the source image (`ghcr.io/stargz-containers/golang:1.15.3-buster-org`) as a container and profiles all file accesses during the execution.
Then these accessed files are marked as "prioritized" files and will be prefetched on runtime.

You can lazy-pull this image with Stargz Snapshotter.
The following example lazily pulls this image to containerd, using `ctr-remote image rpull` command (`http://` prefix isn't needed here).

```console
# ctr-remote image rpull --plain-http registry2:5000/golang:1.15.3-esgz
egistry2:5000/golang:1.15.3-esgz
fetching sha256:bd2e5983... application/vnd.oci.image.index.v1+json
fetching sha256:55aef317... application/vnd.docker.distribution.manifest.v2+json
fetching sha256:d1123476... application/vnd.docker.container.image.v1+json
# ctr-remote run --rm -t --snapshotter=stargz registry2:5000/golang:1.15.3-esgz test echo hello
hello
```

## Optimizing an image with custom configuration

You can also specify the custom workload configuration that the image is optimized against.
The following example optimizes the image against the workload running `go version` on `/bin/bash`.

```
ctr-remote image optimize --plain-http \
           --entrypoint='[ "/bin/bash", "-c" ]' --args='[ "go version" ]' \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           http://registry2:5000/golang:1.15.3-esgz-go-version
```

Other options are also available for configuring the workload.

|Option|Description|
---|---
|`--entrypoint`|Entrypoint of the container (in JSON array)|
|`--args`|Arguments for the entrypoint (in JSON array)|
|`--env`|Environment variables in the container|
|`--user`|User name to run the process in the container|
|`--cwd`|Working directory|
|`--period`|The time seconds during profiling the file accesses|
|`-t`or`--terminal`|Enable to interactively control the container|

## Mounting files from the host

There are several cases where sharing files from host to the container during optimization is useful.
This includes when we want to optimize an image against building a binary using compilers.

For these use-cases, you can mount files on the hosts to the container using `--mount` option.
The following example optimizes the image against the workload where Go compiler compiles "hello world" program.

First, create the following Go source file at `/tmp/hello.go`.

```golang
package main

import "fmt"

func main() {
     fmt.Println("hello world")
}
```

Then you can build it inside the container by bind-mounting the file `/tmp/hello.go` on the host to this container.

```
ctr-remote image optimize --plain-http \
           --mount=type=bind,source=/tmp/hello.go,destination=/hello.go,options=bind:ro \
           --entrypoint='[ "/bin/bash", "-c" ]' --args='[ "go build -o /hello /hello.go && /hello" ]' \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           http://registry2:5000/golang:1.15.3-esgz-hello-world
```

The syntax of the `--mount` option is compatible to containerd's `ctr` tool and [corresponds to the OCI Runtime Spec](https://github.com/opencontainers/runtime-spec/blob/v1.0.2/config.md#mounts).
You need to specify the following key-value pairs with comma separators.

|Field|Description|
---|---
|`type`|The type of the filesystem to be mounted|
|`destination`|The absolute path to the destination mount point in the container|
|`source`|The source of the mount|
|`options`|Mount options (separated by `:`) of the filesystem|

## Enabling CNI-based Networking

You can also gain network connection in the container during optimization, using CNI plugins.
Once you configure CNI plugins on the host, CNI-based networking can be enabled using `--cni` option.

The following example accesses to https://example.com from the container, using `bridge` CNI plugin installed in the demo environment.
```
ctr-remote image optimize --plain-http \
           --cni \
           --entrypoint='[ "/bin/bash", "-c" ]' \
           --args='[ "curl example.com" ]' \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           http://registry2:5000/golang:1.15.3-esgz-curl
```

If CNI plugins and configurations are installed to locations other than well-known paths (/opt/cni/bin and /etc/cni/net.d), you can tell it to `ctr-remote` using the following options.

|Option|Description|
---|---
|`--cni-plugin-config-dir`|Path to the directory where CNI plugins configurations are stored|
|`--cni-plugin-dir`|Path to the directory where CNI plugins binaries are installed|

By default, `/etc/hosts` and `/etc/resolv.conf` are configured as the following.

/etc/hosts

```
127.0.0.1	localhost
::1		localhost ip6-localhost ip6-loopback
fe00::0		ip6-localnet
ff00::0		ip6-mcastprefix
ff02::1		ip6-allnodes
ff02::2		ip6-allrouters
```

/etc/resolv.conf

```
nameserver 8.8.8.8
```

If you want to customize the configuration, the following options are useful.

|Option|Description|
---|---
|`--add-hosts`|Commma-separated list of hosts configuration (formatted as `hostname:IP`) for `/etc/hosts`|
|`--dns-nameservers`|Comma-separated `nameserver` configs added to the container's `/etc/resolv.conf`|
|`--dns-search-domains`|Comma-separated `search` configs added to the container's `/etc/resolv.conf`|
|`--dns-options`|Comma-separated `options` configs added to the container's `/etc/resolv.conf`|

## Image input and output

You can pull the source image and push the result image from/to the following locations.

- Registry
- Local directory

By default, `ctr-remote` recognizes the specified references as pointing to the registry and `~/.docker/config.json` is used for the authentication.
As shown in the above examples, you can also use HTTP registries by using `--plain-http` option and adding `http://` prefix to the reference.

```
ctr-remote image optimize --plain-http \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           http://registry2:5000/golang:1.15.3-esgz
```

You can also pull/push images against local directories.
This is useful especially for debugging including inspecting each layer and metadata blobs directly.
You can specify local paths using `local://` prefix.
The following example stores the result image to the local directory `/tmp/output` in the format of [OCI Image Layout](https://github.com/opencontainers/image-spec/blob/master/image-layout.md).

```
ctr-remote image optimize \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           local:///tmp/output/
```

## Other useful features

### Reusing already-converted layers

If the source image is large, its conversion takes accordingly long time.
But you can skip conversion for layers that are already converted to eStargz.
For enabling this feature, add `--reuse` option to `ctr-remote`.

The following example re-converts already converted eStargz image (`ghcr.io/stargz-containers/golang:1.15.3-buster-esgz`) with `--reuse` feature.

```
ctr-remote image optimize --plain-http \
           --reuse \
           ghcr.io/stargz-containers/golang:1.15.3-buster-esgz \
           http://registry2:5000/golang:1.15.3-esgz
```

You will see `ctr-remote` skips converting some layers with printing `copying without conversion` log messages as the following.

```
(... omit ...)
INFO[0036] no access occur; copying without conversion   digest="sha256:6aedf0c74720e30b9093dc0d2b39c2dd88f35ead14e2087bb49c1608bb151e61"
INFO[0036] no access occur; copying without conversion   digest="sha256:4416ecf7e2787af750fe3b1988f36a2c47edc2d3162739c6eedd739d6d5a14d1"
INFO[0036] no access occur; copying without conversion   digest="sha256:976cc0da952505fede3abe08c0ff0c5277416828c4dff8bd01b306c5b4e5c6f5"
INFO[0036] no access occur; copying without conversion   digest="sha256:79d28aed10b15d548b63eea4cc59e518c4939f9c8fb8498100ec658fc7e0baca"
INFO[0073] converted                                     digest="sha256:62d6dbb94750b1be9f60535bb7d0b1b02c6e9e37d1c55a761c1fc7883ca7e8ad"
INFO[0074] converted                                     digest="sha256:59cf7266511a915072804370a3083a1007c4fb757d800ceef848032ac4a5b605"
INFO[0078] converted                                     digest="sha256:ca2a1da2dee341a3b87a14d56603e9c29c66721056a47bec156f9b04ee0b1e5e"
(... omit ...)
```

When optimizing an image, `ctr-remote` tries to avoid layer conversion as much as possible.
The layers that meet the following conditions are skipped to converting.

- layers that are already formatted as eStargz
- layers that no file access occurred during optimization

### Converting multi-platform images

You can also convert multi-platform images.
If you want to convert all images contained in a multi-platform image, use `--all-platform` option.
If you want to convert an image corresponding to a specific platform, tell it using `--platform` option.
The format of the `--platform` option is `<os>|<arch>|<os>/<arch>[/<variant>]`, please refer to containerd's [godoc](https://godoc.org/github.com/containerd/containerd/platforms#hdr-Platform_Specifiers) for more details.

The following example converts all images contained in `ghcr.io/stargz-containers/golang:1.15.3-buster-org`.

```
ctr-remote image optimize --plain-http \
           --all-platforms \
           ghcr.io/stargz-containers/golang:1.15.3-buster-org \
           http://registry2:5000/golang:1.15.3-esgz-fat
```

By default, when the source image is a multi-platform image, `ctr-remote` converts the image corresponding to the platform where `ctr-remote` runs.

Note that though the images specified by `--all-platform` and `--platform` are converted to eStargz, images that don't correspond to the current platform aren't *optimized*. That is, these images are lazily pulled but without prefetch.
