# Rootless execution of stargz snapshotter

This document lists links and information about how to run Stargz Snapshotter and Stargz Store from the non-root user.

## nerdctl (Stargz Snapshotter)

Rootless Stargz Snapshotter for nerdctl can be installed via `containerd-rootless-setuptool.sh install-stargz` command.
Please see [the doc in nerdctl repo](https://github.com/containerd/nerdctl/blob/v1.1.0/docs/rootless.md#stargz-snapshotter) for details.

## Podman (Stargz Store)

> NOTE: This is an experimental configuration leveraging [`podman unshare`](https://docs.podman.io/en/latest/markdown/podman-unshare.1.html). Limitation: `--uidmap` of `podman run` doesn't work.

First, allow podman using Stargz Store by adding the following store configuration.
Put the configuration file to [`/etc/containers/storage.conf` or `$HOME/.config/containers/storage.conf`](https://github.com/containers/podman/blob/v4.3.1/docs/tutorials/rootless_tutorial.md#storageconf).

> NOTE: Replace `/path/to/home` to the actual home directory.

```
[storage]
driver = "overlay"

[storage.options]
additionallayerstores = ["/path/to/homedir/.local/share/stargz-store/store:ref"]
```

Start Stargz Store in the namespace managed by podman via [`podman unshare`](https://docs.podman.io/en/latest/markdown/podman-unshare.1.html) command.

```
$ podman unshare stargz-store --root $HOME/.local/share/stargz-store/data $HOME/.local/share/stargz-store/store &
```

Podman performs lazy pulling when it pulls eStargz images.

```
$ podman pull ghcr.io/stargz-containers/python:3.9-esgz
```

<details>
<summary>Creating systemd unit file for Stargz Store</summary>

It's possible to create systemd unit file of Stargz Store for easily managing it.
An example systemd unit file can be found [here](../script/podman/config/podman-rootless-stargz-store.service)

After installing that file (e.g. to `$HOME/.config/systemd/user/`), start the service using `systemctl`.

```
$ systemctl --user start podman-rootless-stargz-store
```

</details>

## BuildKit (Stargz Snapshotter)

BuildKit supports running Stargz Snapshotter from the non-root user.
Please see [the doc in BuildKit repo](https://github.com/moby/buildkit/blob/8b132188aa7af944c813d02da63c93308d83cf75/docs/stargz-estargz.md) (unmerged 2023/1/18) for details.
