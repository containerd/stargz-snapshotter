# Getting started with Stargz Snapshotter on Lima

[Lima](https://github.com/lima-vm/lima) is a tool to manage Linux virtual machines on various hosts, including MacOS and Linux.
Lima can be used as an easy way to get started with Stargz Snapshotter as Lima provides a default VM image bundling [containerd](https://github.com/containerd/containerd), [nerdctl](https://github.com/containerd/nerdctl)(Docker-compatible CLI of containerd) and Stargz Snapshotter.

This document describes how to get started with Stargz Snapshotter on Lima.

## Enable Stargz Snapshotter using `--snapshotter=stargz` flag

nerdctl's `--snapshotter=stargz` flag enables stargz-snapshotter.

```
$ nerdctl.lima --snapshotter=stargz system info | grep stargz
 Storage Driver: stargz
```

Using this flag, you can perform lazy pulling of a python eStargz image and run it.

```
$ nerdctl.lima --snapshotter=stargz run --rm -it --name python ghcr.io/stargz-containers/python:3.13-esgz
Python 3.13.2 (main, Feb  6 2025, 22:37:13) [GCC 12.2.0] on linux
Type "help", "copyright", "credits" or "license" for more information.
>>>
```

## Use Stargz Snapshotter as the default snapshotter

nerdctl recognizes an environment variable `CONTAINERD_SNAPSHOTTER` for the snapshotter to use.
You can add this environment variable to the VM by configuring Lima config as shown in the following:

```
$ cat <<EOF >> ~/.lima/_config/override.yaml
env:
  CONTAINERD_SNAPSHOTTER: stargz
EOF
$ limactl stop
$ limactl start
$ nerdctl.lima system info | grep Storage
Storage Driver: stargz
```

> NOTE: `override.yaml` applies to all the instances of Lima


You can perform lazy pulling of eStargz using nerdctl, without any extra flags.

```
$ nerdctl.lima run --rm -it --name python ghcr.io/stargz-containers/python:3.13-esgz
Python 3.13.2 (main, Feb  6 2025, 22:37:13) [GCC 12.2.0] on linux
Type "help", "copyright", "credits" or "license" for more information.
>>>
```
