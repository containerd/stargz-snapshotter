# Enabling Stargz Snapshotter With Transfer Service

Transfer Service is a containerd component which is used for image management in contianerd (e.g. pulling and pushing images).
For details about Transfer Service, refer to [the official document in the containerd repo](https://github.com/containerd/containerd/blob/6af7c07905a317d4c343a49255e2392f4c8569f9/docs/transfer.md).

To use Stargz Snapshotter on containerd with enabling Transfer Service, additional configurations is needed.

## Availability of Transfer Service

Transfer Service is available since v1.7.
And this is enabled in different settings depending on the containerd version.

|containerd version|`ctr`|CRI|
---|---|---
|containerd >= v1.7 and < v2.0|Disabled by default. Enabled by `--local=false`|Disabled|
|containerd >= v2.0 and < v2.1|Enabled by default. Disabled by `--local`|Disabled|
|containerd >= v2.1|Enabled by default. Disabled by `--local`|Enabled by default. Disabled when conditions described in [containerd's CRI document](https://github.com/containerd/containerd/blob/v2.1.0/docs/cri/config.md#image-pull-configuration-since-containerd-v21) are met|

### Note about containerd v2.1

Before containerd v2.1, `disable_snapshot_annotations = false` in containerd's config TOML was a mandatory field to enable Stargz Snapshotter in CRI.
In containerd v2.1, `disable_snapshot_annotations = false` field can still be used to enable Stargz Snapshotter and containerd disables Transfer Service when this field is detected.
If you want to enable Transfer Service, you need to remove `disable_snapshot_annotations = false` field and apply the configuration explaind in this document.

## How to enable Stargz Snapshotter when Transfer Service is enabled?

In containerd v2.1, Transfer Service added support for remote snapshotters like Stargz Snapshotter.

### For ctr and other non-CRI clients

To enable Stargz Snapshotter with Transfer Service, you need to start containerd-stargz-grpc on the node and add the following configuration to contianerd's config TOML file.
Note that you need to add a field `enable_remote_snapshot_annotations = "true"` in `proxy_plugins.stargz.exports` so that containerd can correctly pass image-related information to Stargz Snapshotter.

```toml
version = 2

# Enable Stargz Snapshotter in Transfer Service
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "stargz"

# Plugin Stargz Snapshotter
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
  [proxy_plugins.stargz.exports]
    root = "/var/lib/containerd-stargz-grpc/"
    enable_remote_snapshot_annotations = "true"
```

#### Example client command

When you enable Transfer Service with Stargz Snapshotter, you can perform lazy pulling using the normal `ctr` command. (of course, `ctr-remote` can still be used)

```
# ctr image pull --snapshotter=stargz ghcr.io/stargz-containers/ubuntu:24.04-esgz
```

Then `mount | grep stargz` prints stargz mounts on the node.

### For CRI

To enable Stargz Snapshotter with Transfer Service, you need to start containerd-stargz-grpc on the node and add the following configuration to contianerd's config TOML file.

```toml
version = 2

# Basic CRI configuration with enabling Stargz Snapshotter
[plugins."io.containerd.grpc.v1.cri".containerd]
  default_runtime_name = "runc"
  snapshotter = "stargz"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"

# Enable Stargz Snapshotter in Transfer Service
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux"
  snapshotter = "stargz"

# Plugin Stargz Snapshotter
[proxy_plugins]
  [proxy_plugins.stargz]
    type = "snapshot"
    address = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
  [proxy_plugins.stargz.exports]
    root = "/var/lib/containerd-stargz-grpc/"
    enable_remote_snapshot_annotations = "true"
```

#### Example client command

You can quickly check the behaviour using `crictl` command.

```
# crictl image pull ghcr.io/stargz-containers/ubuntu:24.04-esgz
```

Then `mount | grep stargz` prints stargz mounts on the node.
