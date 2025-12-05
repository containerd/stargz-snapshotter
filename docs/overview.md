# Containerd Stargz Snapshotter Plugin Overview

__Before reading this overview document, we recommend you read [README](../README.md).__

Pulling images is one of the most time-consuming steps in the container startup process.
In the containerd community, we have had a lot of discussions to address this issue at the following:

- [#3731 Support remote snapshotter to speed up image pulling](https://github.com/containerd/containerd/issues/3731)
- [#2968 Support `Prepare` for existing snapshots in Snapshotter interface](https://github.com/containerd/containerd/issues/2968)
- [#2943 remote filesystem snapshotter](https://github.com/containerd/containerd/issues/2943)

The solution for fast image distribution is called *Remote Snapshotter* plugin.
This prepares the container's rootfs layers by directly mounting from remote stores instead of downloading and unpacking the entire image contents.
The actual image contents can be fetched *lazily* so runtimes can start containers before the entire image contents are locally available.
We call these remotely mounted layers *remote snapshots*.

*Stargz Snapshotter* is a remote snapshotter plugin implementation which supports standard compatible remote snapshots functionality.
This snapshotter leverages [eStargz](/docs/stargz-estargz.md) image, which is lazily-pullable and still standard-compatible.
Because of this compatibility, eStargz images can be pushed to and lazily pulled from [OCI](https://github.com/opencontainers/distribution-spec)/[Docker](https://docs.docker.com/registry/spec/api/) registries (e.g. ghcr.io).
Furthermore, images can run even on eStargz-agnostic runtimes (e.g. Docker).
When you run a container image and it is formatted by eStargz, stargz snapshotter prepares container's rootfs layers as remote snapshots by mounting layers from the registry to the node, instead of pulling the entire image contents.

This document gives you a high-level overview of stargz snapshotter.

![overview](/docs/images/overview01.png)

## Stargz Snapshotter proxy plugin

Stargz snapshotter is implemented as a [proxy plugin](https://github.com/containerd/containerd/blob/04985039cede6aafbb7dfb3206c9c4d04e2f924d/PLUGINS.md#proxy-plugins) daemon (`containerd-stargz-grpc`) for containerd.
When containerd starts a container, it queries the rootfs snapshots to stargz snapshotter daemon through a unix socket.
This snapshotter remotely mounts queried eStargz layers from registries to the node and provides these mount points as remote snapshots to containerd.

Containerd recognizes this plugin through a unix socket specified in the configuration file (e.g. `/etc/containerd/config.toml`).
Stargz snapshotter can also be used through Kubernetes CRI by specifying the snapshotter name in the CRI plugin configuration.
We assume that you are using containerd (> v1.4.2).

```toml
version = 2

# Plug stargz snapshotter into containerd
# Containerd recognizes stargz snapshotter through specified socket address.
# The specified address below is the default which stargz snapshotter listen to.
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
  [proxy_plugins.stargz.exports]
    root = "/var/lib/containerd-stargz-grpc/"

# Use stargz snapshotter through CRI
[plugins."io.containerd.grpc.v1.cri".containerd]
  snapshotter = "stargz"
  disable_snapshot_annotations = false
```

> NOTE: `root` field of `proxy_plugins` is needed for the CRI plugin to recognize stargz snapshotter's root directory.

This repo contains [a Dockerfile as a KinD node image](/Dockerfile) which includes the above configuration.

## State directory

Stargz snapshotter mounts eStargz layers from registries to the node using FUSE.
Metadata for all files in the image are preserved on the container filesystem and the file contents are fetched from registries on demand.

At the root of the container filesystem, there is a *state directory* (`/.stargz-snapshotter`) for status monitoring for the filesystem.
This directory is hidden from `getdents(2)` so you can't see this with `ls -a /`.
Instead, you can directly access the directory by specifying the path (`/.stargz-snapshotter`).

The state directory contains JSON-formatted metadata files for each layer.
In the following example, metadata JSON files for overlayed 7 layers are visible.
In each metadata JSON file, the following fields are contained:

- `digest` contains the layer digest. This is the same value as that in the image's manifest.
- `size` is the size bytes of the layer.
- `fetchedSize` and `fetchedPercent` indicate how many bytes have been fetched for this layer. Stargz snapshotter aggressively downloads this layer in the background - unless configured otherwise - so these values gradually increase. When `fetchedPercent` reaches `100` percent, this layer has been fully downloaded on the node and no further access will occur for reading files.

Note that the state directory layout and the metadata JSON structure are subject to change.

```console
# ctr-remote run --rm -t --snapshotter=stargz docker.io/stargz/golang:1.12.9-esgz test /bin/bash
root@1d43741b8d29:/go# ls -a /
.   bin   dev  go    lib    media  opt     root  sbin  sys  usr
..  boot  etc  home  lib64  mnt    proc  run   srv   tmp  var
root@1d43741b8d29:/go# ls /.stargz-snapshotter/*
/.stargz-snapshotter/sha256:2b1fc65cafe05b65acc9e9f186df4dd81ae74c58ef73d89ecfc15e7286b3e960.json
/.stargz-snapshotter/sha256:42d56485c1f672e394a02855048774621731c8fd44a54dc816a421a3a52b8482.json
/.stargz-snapshotter/sha256:6a5826d877de5c93fb4a9e1d0369cfdef6d43df2610562501ebf42e4bcb2ef73.json
/.stargz-snapshotter/sha256:a4d35801573274df19d9c2ae2aed80eba96d5aa69a38c464e1f01f9abf81e34e.json
/.stargz-snapshotter/sha256:ab13100112faac6e04d2da2281db3df942efc8cef2532ab2cac688c6232944d8.json
/.stargz-snapshotter/sha256:e8cc31024eb09fe216ad906392aec139038330c6d29dfd3fe5c81c4b2dd21430.json
/.stargz-snapshotter/sha256:f077511be7d385c17ba88980379c5cd0aab7068844dffa7a1cefbf68cc3daea3.json
root@1d43741b8d29:/go# cat /.stargz-snapshotter/*
{"digest":"sha256:2b1fc65cafe05b65acc9e9f186df4dd81ae74c58ef73d89ecfc15e7286b3e960","size":131339690,"fetchedSize":7939690,"fetchedPercent":6.045156646859757}
{"digest":"sha256:42d56485c1f672e394a02855048774621731c8fd44a54dc816a421a3a52b8482","size":10047608,"fetchedSize":2047608,"fetchedPercent":20.379059374131632}
{"digest":"sha256:6a5826d877de5c93fb4a9e1d0369cfdef6d43df2610562501ebf42e4bcb2ef73","size":54352828,"fetchedSize":2302828,"fetchedPercent":4.236813584014432}
{"digest":"sha256:a4d35801573274df19d9c2ae2aed80eba96d5aa69a38c464e1f01f9abf81e34e","size":70359295,"fetchedSize":2259295,"fetchedPercent":3.211082487395588}
{"digest":"sha256:ab13100112faac6e04d2da2281db3df942efc8cef2532ab2cac688c6232944d8","size":7890588,"fetchedSize":2140588,"fetchedPercent":27.12837116828302}
{"digest":"sha256:e8cc31024eb09fe216ad906392aec139038330c6d29dfd3fe5c81c4b2dd21430","size":52934435,"fetchedSize":2634435,"fetchedPercent":4.976788738748227}
{"digest":"sha256:f077511be7d385c17ba88980379c5cd0aab7068844dffa7a1cefbf68cc3daea3","size":580,"fetchedSize":580,"fetchedPercent":100}
```

## Fuse Manager

The fuse manager is designed to maintain the availability of running containers by managing the lifecycle of FUSE mountpoints independently from the stargz snapshotter.

### Fuse Manager Overview
Remote snapshots are mounted using FUSE, and its filesystem processes are attached to the stargz snapshotter. If the stargz snapshotter restarts (due to configuration changes or crashes), all filesystem processes will be killed and restarted, which causes the remount of FUSE mountpoints, making running containers unavailable.

To avoid this, we use a fuse daemon called the fuse manager to handle filesystem processes. The fuse manager is responsible for mounting and unmounting remote snapshotters. Its process is detached from the stargz snapshotter main process to an independent one in a shim-like way during the snapshotter's startup. This design ensures that the restart of the snapshotter won't affect the filesystem processes it manages, keeping mountpoints and running containers available during the restart. However, it is important to note that the restart of the fuse manager itself triggers a remount, so it is recommended to keep the fuse manager running in a good state.

You can enable the fuse manager by adding the following configuration.

```toml
[fuse_manager]
enable = true
# address must be an absolute path; default is "/run/containerd-stargz-grpc/fuse-manager.sock"
address = "/run/containerd-stargz-grpc/fuse-manager.sock"
# set a custom binary path if the executable is not in PATH
path = "/usr/local/bin/stargz-fuse-manager"
```

## Killing and restarting Stargz Snapshotter

Stargz Snapshotter works as a FUSE server for the snapshots.
When you stop Stargz Sanpshotter on the node, it takes the following behaviour depending on the configuration.

### FUSE manager mode is disabled

killing containerd-stargz-grpc will result in unmounting all snapshot mounts managed by Stargz Snapshotter.
When containerd-stargz-grpc is restarted, all those snapshots are mounted again by lazy pulling all layers.
If the snapshotter fails to mount one of the snapshots (e.g. because of lazy pulling failure) during this step, the behaviour differs depending on `allow_invalid_mounts_on_restart` flag in the config TOML.

- `allow_invalid_mounts_on_restart = true`: containerd-stargz-grpc leaves the failed snapshots as empty directories. The user needs to manually remove those snapshot via containerd (e.g. using `ctr snapshot rm` command). The name of those snapshots can be seen in the log with `failed to restore remote snapshot` message.

- `allow_invalid_mounts_on_restart = false`: containerd-stargz-grpc doesn't start. The user needs to manually recover this (e.g. by wiping snapshotter and containerd state).

### FUSE manager mode is enabled

Killing containerd-stargz-grpc using non-SIGINT signal (e.g. using SIGTERM) doesn't affect the snapshot mounts because the FUSE manager process detached from containerd-stargz-grpc keeps on serving FUSE mounts to the kernel.
This is useful when you reload the updated config TOML to Stargz Snapshotter without unmounting existing snapshots.

FUSE manager serves FUSE mounts of the snapshots so if you kill this process, all snapshot mounts will be unavailable.
When stopping FUSE manager for upgrading the binary or restarting the node, you can use SIGINT signal to trigger the graceful exit as shown in the following steps.

1. Stop containers that use Stargz Snapshotter. Stopping FUSE manager makes all snapshot mounts unavailable so containers can't keep working.
2. Stop containerd-stargz-grpc process using SIGINT. This signal triggers unmounting of all snapshots and cleaning up of the associated resources.
3. Kill the FUSE manager process (`stargz-fuse-manager`)
4. Restart the containerd-stargz-grpc process. This restores all snapshot mounts by lazy pulling them. `allow_invalid_mounts_on_restart` (described in the above) can still be used for controlling the behaviour of the error cases.
5. Restart the containers.

### Unexpected restart handling

When Stargz Snapshotter is killed unexpectedly (e.g., by OOM killer or system crash), the process doesn't get a chance to perform graceful cleanup. In such cases, the snapshotter can successfully restart and restore remote snapshots, but this may lead to fscache duplicating cached data.

**Recommended handling:**

Since this scenario is caused by abnormal exit, users are expected to manually clean up the cache directory after an unexpected restart to avoid cache duplication issues. The cache cleanup should be performed before restarting the snapshotter service.

## Registry-related configuration

You can configure stargz snapshotter for accessing registries with custom configurations.
The config file must be formatted with TOML and can be passed to stargz snapshotter with `--config` option.

### Authentication

Stargz snapshotter doesn't share private registries creds with containerd.
Instead, this supports authentication in the following methods,

- Using `$DOCKER_CONFIG` or `~/.docker/config.json`
- Proxying and scanning CRI Image Service API
- Using Kubernetes secrets (type = `kubernetes.io/dockerconfigjson`)

#### dockerconfig-based authentication

By default, This snapshotter tries to get creds from `$DOCKER_CONFIG` or `~/.docker/config.json`.
Following example enables stargz snapshotter to access to private registries using `docker login` command. [`nerdctl login`](https://github.com/containerd/nerdctl) can also be used for this.
Stargz snapshotter doesn't share credentials with containerd so credentials specified by `ctr-remote`'s `--user` option in the example is just for containerd.

```console
# docker login
(Enter username and password)
# ctr-remote image rpull --user <username>:<password> docker.io/<your-repository>/ubuntu:18.04
```

#### CRI-based authentication

Following configuration (typically located at `/etc/containerd-stargz-grpc/config.toml`) enables stargz snapshotter to pull private images on Kubernetes.
The snapshotter works as a proxy of CRI Image Service and exposes CRI Image Service API on the snapshotter's unix socket (i.e. `/run/containerd-stargz-grpc/containerd-stargz-grpc.sock`).
The snapshotter acquires registry creds by scanning requests.

You must specify `--image-service-endpoint=unix:///run/containerd-stargz-grpc/containerd-stargz-grpc.sock` option to kubelet.

You can specify the backing image service's socket using `image_service_path`.
The default is the containerd's socket (`/run/containerd/containerd.sock`).

```toml
# Stargz Snapshotter proxies CRI Image Service into containerd socket.
[cri_keychain]
enable_keychain = true
image_service_path = "/run/containerd/containerd.sock"
```

The default path where containerd-stargz-grpc serves the CRI Image Service API is `unix:///run/containerd-stargz-grpc/containerd-stargz-grpc.sock`.
You can also change this path using `listen_path` field.

> Note that if you enabled the FUSE manager and CRI-based authentication together, `listen_path` is a mandatory field with some caveats:
> - This path must be different from the FUSE manager's socket path (`/run/containerd-stargz-grpc/fuse-manager.sock`) because they have different lifecycle. Specifically, the CRI socket is recreted on each reload of the configuration to the FUSE manager.
> - containerd-stargz-grpc's socket path (`/run/containerd-stargz-grpc/containerd-stargz-grpc.sock`) can't be used as `listen_path` because the CRI socket is served by the FUSE manager process (not containerd-stargz-grpc process).

#### kubeconfig-based authentication

This is another way to enable lazy pulling of private images on Kubernetes.

Following configuration (typically located at `/etc/containerd-stargz-grpc/config.toml`) enables stargz snapshotter to access to private registries using kubernetes secrets (type = `kubernetes.io/dockerconfigjson`) in the cluster using kubeconfig files.
You can specify the path of kubeconfig file using `kubeconfig_path` option.
It's no problem that the specified file doesn't exist when this snapshotter starts.
In this case, snapsohtter polls the file until actually provided.
This is useful for some environments (e.g. single node cluster with containerized apiserver) where stargz snapshotter needs to start before everything, including booting containerd/kubelet/apiserver and configuring users/roles.
If no `kubeconfig_path` is specified, snapshotter searches kubeconfig files from `$KUBECONFIG` or `~/.kube/config`.

```toml
# Use Kubernetes secrets accessible by the kubeconfig `/etc/kubernetes/snapshotter/config.conf`.
[kubeconfig_keychain]
enable_keychain = true
kubeconfig_path = "/etc/kubernetes/snapshotter/config.conf"
```

Please note that kubeconfig-based authentication requires additional privilege (i.e. kubeconfig to list/watch secrets) to the node.
And this doesn't work if kubelet retrieve creds from somewhere not API server (e.g. [credential provider](https://kubernetes.io/docs/tasks/kubelet-credential-provider/kubelet-credential-provider/)).

### Registry mirrors and insecure connection

You can also configure mirrored registries and insecure connection.
The hostname used as a mirror host can be specified using `host` option.
If an optional field `insecure` is `true`, snapshotter tries to connect to the registry using plain HTTP instead of HTTPS.

```toml
# Use `mirrorhost.io` as a mirrored host of `exampleregistry.io` and
# use plain HTTP for connecting to the mirror host.
[[resolver.host."exampleregistry.io".mirrors]]
host = "mirrorhost.io"
insecure = true

# Use plain HTTP for connecting to `exampleregistry.io`.
[[resolver.host."exampleregistry.io".mirrors]]
host = "exampleregistry.io"
insecure = true
```

`header` field allows to set headers to send to the server.

```toml
[[resolver.host."registry2:5000".mirrors]]
  host = "registry2:5000"
  [resolver.host."registry2:5000".mirrors.header]
    x-custom-2 = ["value3", "value4"]
```

> NOTE: Headers aren't passed to the redirected location.

The config file can be passed to stargz snapshotter using `containerd-stargz-grpc`'s `--config` option.

## Configuration hot reload

[Fs configurations](/fs/config/config.go) supports hot reloading. When the configuration file is modified, the snapshotter detects the change and applies the new configuration without restarting the process.

Note that other configurations (e.g. `proxy_plugins`, `fuse_manager`, `resolver`, `mount_options`) require a restart to take effect.
Also, some specific fields in `[stargz]` section (e.g. `no_prometheus`) do not support hot reloading and changes to them will be ignored until restart.

## Make your remote snapshotter

It isn't difficult for you to implement your remote snapshotter using [our general snapshotter package](/snapshot) without considering the protocol between that and containerd.
You can configure the remote snapshotter with your `FileSystem` structure which you want to use as a backend filesystem.
[Our snapshotter command](/cmd/containerd-stargz-grpc/main.go) is a good example for the integration.
