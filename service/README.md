# Note about importing of `github.com/containerd/stargz-snapshotter/service/plugin`

`github.com/containerd/stargz-snapshotter/service/plugin` registeres our stargz snapshotter plugin to the containerd plugin system.
This depends on containerd newer than [234bf990dca4e81e89f549448aa6b555286eaa7a](https://github.com/containerd/containerd/commit/234bf990dca4e81e89f549448aa6b555286eaa7a) which contains CRI v1alpha API fork.

If you import this plugin to the tool that imports older version of containerd, import `github.com/containerd/stargz-snapshotter/service/pluginforked` instead.
This plugin contains our forked CRI v1alpha API.

Once you upgrade containerd to newer than 234bf990dca4e81e89f549448aa6b555286eaa7a, you can't use `github.com/containerd/stargz-snapshotter/service/pluginforked` and protobuf code fails with the error e.g. `proto: duplicate enum registered: runtime.v1alpha2.Protocol`. In this case, use `github.com/containerd/stargz-snapshotter/service/plugin`.
