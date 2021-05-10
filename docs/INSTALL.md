# Install Stargz Snapshotter

We assume that you are using containerd (> v1.4.2) as a CRI runtime.

## Install Stargz Snapshotter with Systemd

- Download release tarball from [the release page](https://github.com/containerd/stargz-snapshotter/releases). For example, amd64 binary of v0.5.0 is available from https://github.com/containerd/stargz-snapshotter/releases/download/v0.5.0/stargz-snapshotter-v0.5.0-linux-amd64.tar.gz.

- Add the following configuration to containerd's configuration file (typically: /etc/containerd/config.toml). Please see also [an example configuration file](../script/config/etc/containerd/config.toml).
  ```
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
    tar -xvf stargz-snapshotter-${version}-linux-${arch}.tar.gz -C /usr/local/bin
    wget -O /etc/systemd/system/stargz-snapshotter.service https://raw.githubusercontent.com/containerd/stargz-snapshotter/master/script/config/etc/systemd/system/stargz-snapshotter.service
    systemctl enable --now stargz-snapshotter
    systemctl restart containerd
  ```