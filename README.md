# Stargz Snapshotter

[![Tests Status](https://github.com/containerd/stargz-snapshotter/workflows/Tests/badge.svg)](https://github.com/containerd/stargz-snapshotter/actions?query=workflow%3ATests+branch%3Amaster)
[![Benchmarking](https://github.com/containerd/stargz-snapshotter/workflows/Benchmark/badge.svg)](https://github.com/containerd/stargz-snapshotter/actions?query=workflow%3ABenchmark+branch%3Amaster)

Pulling image is one of the time-consuming steps in the container lifecycle. Research shows that time to take for pull operation accounts for 76% of container startup time[[FAST '16]](https://www.usenix.org/node/194431). *Stargz Snapshotter* is an implementation of snapshotter which aims to solve this problem leveraging [stargz image format by CRFS](https://github.com/google/crfs). The following histogram is the benchmarking result for startup time of several containers measured on Github Actions, using Docker Hub as a registry.

<img src="docs/images/benchmarking-result-288c338.png" width="600" alt="The benchmarking result on 288c338">

`legacy` shows the startup performance when we use containerd's default snapshotter (`overlayfs`) with images copied from `docker.io/library` without optimization. For this configuration, containerd pulls entire image contents and `pull` operation takes accordingly. When we use stargz snapshotter with `stargz` images we are seeing performance improvement on the `pull` operation because containerd can start the container before the entire image contents locally available and fetches each file on-demand. But at the same time, we see the performance drawback for `run` operation because each access to files takes extra time for fetching them from the registry. When we use the further optimized version of images(`estargz`) we can mitigate the performance drawback observed in `stargz` images. This is because stargz snapshotter prefetches and caches some files which will be most likely accessed during container workload. Stargz snapshotter waits for the first container creation until the prefetch completes so `create` sometimes takes longer than other types of image. But this wait only occurs just after the pull completion until the prefetch completion and it's shorter than waiting for downloading all files of all layers.

The above histogram is [the benchmarking result on the commit `288c338`](https://github.com/containerd/stargz-snapshotter/actions/runs/50632674). We are constantly measuring the performance of this snapshotter so you can get the latest one through the badge shown top of this doc. Please note that we sometimes see dispersion among the results because of the NW condition on the internet and/or the location of the instance in the Github Actions, etc. Our benchmarking method is based on [HelloBench](https://github.com/Tintri/hello-bench).

Stargz Snapshotter is a **non-core** sub-project of containerd.

## Demo

You can test this snapshotter with the latest containerd. Though we still need patches on clients and we are working on, you can use [a customized version of ctr command](cmd/ctr-remote) for a quick tasting. For an overview of the snapshotter, please check [this doc](./docs/overview.md).

### Build and run the environment

First, please put this repo on your `GOPATH`(`${GOPATH}/src/github.com/containerd/stargz-snapshotter`).

```console
$ cd ${GOPATH}/src/github.com/containerd/stargz-snapshotter/script/demo
$ docker-compose build --build-arg HTTP_PROXY=$HTTP_PROXY \
                       --build-arg HTTPS_PROXY=$HTTP_PROXY \
                       --build-arg http_proxy=$HTTP_PROXY \
                       --build-arg https_proxy=$HTTP_PROXY \
                       containerd_demo
$ docker-compose up -d
$ docker exec -it containerd_demo /bin/bash
(inside container) # ./script/demo/run.sh
```

### Prepare stargz-formatted image on a registry

For making and pushing a stargz image, you can use CRFS-official `stargzify` command or our `ctr-remote` command which has further optimization functionality. In this example, we use `ctr-remote`. You can also try our pre-converted images listed in [this doc](./docs/pre-converted-images.md).

We optimize the image for speeding up the execution of `ls` command on `bash`.
```console
# ctr-remote image optimize --plain-http --entrypoint='[ "/bin/bash", "-c" ]' --args='[ "ls" ]' \
             ubuntu:18.04 http://registry2:5000/ubuntu:18.04
```
The converted image is still __docker-compatible__ so you can push/pull/run it with existing tools (e.g. docker).

### Run the container with stargz snapshots

Layer downloads don't occur. So this "pull" operation ends soon.
```console
# time ctr-remote images rpull --plain-http registry2:5000/ubuntu:18.04
fetching sha256:728332a6... application/vnd.docker.distribution.manifest.v2+json
fetching sha256:80026893... application/vnd.docker.container.image.v1+json

real	0m0.176s
user	0m0.025s
sys	0m0.005s
# ctr-remote run --rm -t --snapshotter=stargz registry2:5000/ubuntu:18.04 test /bin/bash
root@8dab301bd68d:/# ls
bin  boot  dev  etc  home  lib  lib64  media  mnt  opt  proc  root  run  sbin  srv  sys  tmp  usr  var
```

## Authentication

We support the following methods for private repository authentication.
- Using `DOCKER_CONFIG` or `~/.docker/config.json`
- Using Kubernetes secrets (type = `kubernetes.io/dockerconfigjson`)

Following example enables stargz snapshotter to access to private registries using `docker login` command. Stargz snapshotter gets credentials from `DOCKER_CONFIG`(or `~/.docker/config.json`).
```console
# docker login
(Enter username and password)
# ctr-remote image rpull --user <username>:<password> docker.io/<your-repository>/ubuntu:18.04
```

Following configuration enables stargz snapshotter to access to private registries using kubernetes secrets (type = `kubernetes.io/dockerconfigjson`) in the cluster using kubeconfig files. You can specify the path of kubeconfig file to use with `kubeconfig_path` option. It's no problem that the specified file doesn't exist when this snapshotter starts. In this case, snapsohtter polls the file until actually provided. This is useful for some environments (e.g. single node cluster with containerized apiserver) where stargz snapshotter needs to start before everything, including booting containerd/kubelet/apiserver and configuring users/roles. If no `kubeconfig_path` is specified, snapshotter searches kubeconfig files from `KUBECONFIG` or `~/.kube/config`.
```toml
[kubeconfig_keychain]
enable_keychain = true
kubeconfig_path = "/etc/kubernetes/snapshotter/config.conf"
```

We don't share credentials with containerd so credentials specified by ctr's `--user` option in the above example is just for containerd's side. If you have no right to access to the repository with credentials specified to stargz snapshotter, pull operations fall back to the normal one(i.e. overlayfs).

## Project details

Stargz Snapshotter is a containerd **non-core** sub-project, licensed under the [Apache 2.0 license](./LICENSE).
As a containerd non-core sub-project, you will find the:
 * [Project governance](https://github.com/containerd/project/blob/master/GOVERNANCE.md),
 * [Maintainers](./MAINTAINERS),
 * and [Contributing guidelines](https://github.com/containerd/project/blob/master/CONTRIBUTING.md)

information in our [`containerd/project`](https://github.com/containerd/project) repository.
