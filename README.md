# Remote Snapshotter

Pulling image is one of the major performance bottlenecks in container workload. Research shows that time for pulling accounts for 76% of container startup time[[FAST '16]](https://www.usenix.org/node/194431). *Remote snapshotter* is a solution discussed in the containerd community and this implementation is based on it.

Related discussion of the snapshotter in containerd community:
- [Support remote snapshotter to speed up image pulling#3731@containerd](https://github.com/containerd/containerd/issues/3731)
- [Support `Prepare` for existing snapshots in Snapshotter interface#2968@containerd](https://github.com/containerd/containerd/issues/2968)
- [remote filesystem snapshotter#2943@containerd](https://github.com/containerd/containerd/issues/2943)

By using this snapshotter, images(even if they are huge) can be pulled in lightning speed because this skips pulling layers but fetches the contents on demand at runtime.
```
# time ctr images pull --plain-http --skip-download --snapshotter=remote registry2:5000/fedora:30 > /dev/null 
real	0m0.447s
user	0m0.081s
sys	0m0.019s
# time ctr images pull --plain-http --skip-download --snapshotter=remote registry2:5000/python:3.7 > /dev/null 
real	0m1.041s
user	0m0.073s
sys	0m0.028s
# time ctr images pull --plain-http --skip-download --snapshotter=remote registry2:5000/jenkins:2.60.3 > /dev/null 
real	0m1.231s
user	0m0.112s
sys	0m0.008s
```
To achive that we supports following [filesystems](filesystems):
- Filesystem using [stargz formatted image introduced by CRFS](https://github.com/google/crfs), which is compatible with current docker image format.

This snapshotter can be plugged into [patched version of containerd](https://github.com/ktock/containerd/tree/filter-by-snapshotter-test) but we are currently working on containerd side to make this work with containerd without patches.

## demo

__NOTICE:__

- Put this repo on your GOPATH(${GOPATH}/src/github.com/ktock/remote-snapshotter).

### Build and run the environment
```
$ cd ${GOPATH}/src/github.com/ktock/remote-snapshotter/demo
$ docker-compose build --build-arg HTTP_PROXY=$HTTP_PROXY \
                       --build-arg HTTPS_PROXY=$HTTP_PROXY \
                       --build-arg http_proxy=$HTTP_PROXY \
                       --build-arg https_proxy=$HTTP_PROXY \
                       containerd
$ docker-compose up -d
$ docker exec -it containerd /bin/bash
(inside container) # /run.sh
```

### Prepare stargz-formatted image on a registry

Use [stargzify](https://github.com/google/crfs/tree/master/stargz/stargzify) command to convert the image.
```
# stargzify -insecure ubuntu:18.04 http://registry2:5000/ubuntu:18.04
```
The converted image is still __compatible with a normal docker image__ so you can still pull and run it with normal tools(e.g. docker).

### Pull the image without downloading layers(it's sometimes called "lazypull") and run it
```
# time ctr images pull --plain-http --skip-download --snapshotter=remote registry2:5000/ubuntu:18.04
(Layer downloads don't occur. So this "pull" operation will end in around 1 sec.)
real	0m0.248s
user	0m0.020s
sys	0m0.011s
# ctr run --rm -t --snapshotter=remote registry2:5000/ubuntu:18.04 test /bin/bash
root@8dab301bd68d:/# ls
bin  boot  dev  etc  home  lib  lib64  media  mnt  opt  proc  root  run  sbin  srv  sys  tmp  usr  var
```

### Cleanup
```
# ctr t kill -s 9 test
# ctr c rm test
```

## Authentication

We support private repository authentication powerd by [go-containerregistry](https://github.com/google/go-containerregistry) which supports `~/.docker/config.json`-based credential management.
You can authenticate yourself with normal operations (i.e. `docker login` command) using `~/.docker/config.json`.

In the example showed above, you can pull images from your private repository on the DockerHub:
```
# docker login
(Enter username and password)
# ctr image pull --user <username>:<password> --skip-download --snapshotter remote index.docker.io/<your-repository>/ubuntu:18.04
```
The `--user` option is just for containerd's side which doesn't recognize `~/.docker/config.json`.
We doesn't use credentials specified by this option but uses `~/.docker/config.json` instead.
If you have no right to access the repository with credentials stored in `~/.docker/config.json`, this pull optration fallbacks to the normal one(i.e. overlayfs).

## Filesystem integration

Filesystems can be easily integrated with this snapshotter and containerd by implementing a simple interface described [here](filesystems/plugin.go) without thinking about remote snapshotter protocol. See [the existing implementation](filesystems/stargz/fs.go).

# TODO

## General issues:
- [ ] Completing necessary patches on the containerd.
  - [x] Implement the protocol on metadata snapshotter: https://github.com/containerd/containerd/pull/3793
  - [ ] Skip downloading remote snapshot layers: https://github.com/containerd/containerd/pull/3846
  - [ ] Deal with ErrUnavailable error and try re-pull layers

## Snapshotter specific issues:
- [x] Resiliency:
  - [x] Ensure all mounts are available on every Prepare() and report erros when unavailable.
  - [x] Deal with runtime problems(NW disconnection, authn failure and so on).
- [x] Authn: Implement fundamental private repository authentication using `~/.docker/config.json`.
- [ ] Performance: READ performance improvement
- [ ] Documentation: ([architecture.md](architecture.md)) is stale.
