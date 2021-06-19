# Install Stargz Snapshotter and Stargz Store

## What's Stargz Snapshotter and Stargz Store?

*Stargz Snapshotter* is a plugin for containerd, which enables it to perform lazy pulling of eStargz.
This is an implementation of *remote snapshotter* plugin and provides remotely-mounted eStargz layers to containerd.
Communication between containerd and Stargz Snapshotter is done with gRPC over unix socket.
For more details about Stargz Snapshotter and the relationship with containerd, [please refer to the doc](./overview.md).

If you are using CRI-O/Podman, you can't use Stargz Snapshotter for enabling lazy pulling of eStargz.
Instead, use *Stargz Store* plugin.
This is an implementation of *additional layer store* plugin of CRI-O/Podman.
Stargz Store provides remotely-mounted eStargz layers to CRI-O/Podman.

Stargz Store exposes mounted filesystem structured like the following.
CRI-O/Podman access to this filesystem to acquire eStargz layers.

```
<mountpoint>/base64(imageref)/<layerdigest>/
- diff  : exposes the extracted eStargz layer
- info  : contains JSON-formatted metadata of this layer
- use   : files to notify the use of this layer (used for GC)
```

## Install Stargz Snapshotter for containerd with Systemd

To enable lazy pulling of eStargz on containerd, you need to install *Stargz Snapshotter* plugin.
This section shows the step to install Stargz Snapshotter with systemd.
We assume that you are using containerd (> v1.4.2) as a CRI runtime.

- Download release tarball from [the release page](https://github.com/containerd/stargz-snapshotter/releases). For example, amd64 binary of v0.6.0 is available from https://github.com/containerd/stargz-snapshotter/releases/download/v0.6.0/stargz-snapshotter-v0.6.0-linux-amd64.tar.gz.

- Add the following configuration to containerd's configuration file (typically: /etc/containerd/config.toml). Please see also [an example configuration file](../script/config/etc/containerd/config.toml).
  ```toml
  version = 2

  # Enable stargz snapshotter for CRI
  [plugins."io.containerd.grpc.v1.cri".containerd]
    snapshotter = "stargz"
    disable_snapshot_annotations = false

  # Plug stargz snapshotter into containerd
  [proxy_plugins]
    [proxy_plugins.stargz]
      type = "snapshot"
      address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"

  ```

- Install fuse

  ###### centos
  ```
  # centos 7
  yum install fuse
  # centos 8
  dnf install fuse

  modprobe fuse
  ```

  ###### ubuntu

  ```
  apt-get install fuse
  modprobe fuse
  ```

- Start stargz-snapshotter and restart containerd
  ```
  tar -C /usr/local/bin -xvf stargz-snapshotter-${version}-linux-${arch}.tar.gz containerd-stargz-grpc ctr-remote
  wget -O /etc/systemd/system/stargz-snapshotter.service https://raw.githubusercontent.com/containerd/stargz-snapshotter/main/script/config/etc/systemd/system/stargz-snapshotter.service
  systemctl enable --now stargz-snapshotter
  systemctl restart containerd
  ```

## Install Stargz Store for CRI-O/Podman with Systemd

To enable lazy pulling of eStargz on CRI-O/Podman, you need to install *Stargz Store* plugin.
This section shows the step to install Stargz Store with systemd.
We assume that you are using CRI-O newer than https://github.com/cri-o/cri-o/pull/4850 or Podman newer than https://github.com/containers/podman/pull/10214 .

- Download release tarball from [the release page](https://github.com/containerd/stargz-snapshotter/releases). For example, amd64 binary of v0.6.0 is available from https://github.com/containerd/stargz-snapshotter/releases/download/v0.6.0/stargz-snapshotter-v0.6.0-linux-amd64.tar.gz.

- Add the following configuration to the storage configuration file of CRI-O/Podman (typically: /etc/containers/storage.conf). Please see also [an example configuration file](../script/config-cri-o/etc/containers/storage.conf).
  ```toml
  [storage]
  driver = "overlay"
  graphroot = "/var/lib/containers/storage"
  runroot = "/run/containers/storage"

  [storage.options]
  additionallayerstores = ["/var/lib/stargz-store/store:ref"]
  ```

- Install fuse

  ###### centos
  ```
  # centos 7
  yum install fuse
  # centos 8
  dnf install fuse

  modprobe fuse
  ```

  ###### ubuntu

  ```
  apt-get install fuse
  modprobe fuse
  ```

- Start stargz-store (CRI-O also needs to be restarted if you are using)
  ```
  tar -C /usr/local/bin -xvf stargz-snapshotter-${version}-linux-${arch}.tar.gz stargz-store
  wget -O /etc/systemd/system/stargz-store.service https://raw.githubusercontent.com/containerd/stargz-snapshotter/main/script/config-cri-o/etc/systemd/system/stargz-store.service
  systemctl enable --now stargz-store
  systemctl restart cri-o # if you are using CRI-O
  ```
