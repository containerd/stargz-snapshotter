# Introduction

FUSE Passthrough has been introduced in the Linux kernel version 6.9 ([Linux Kernel Commit](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/commit/?id=6ce8b2ce0d7e3a621cdc9eb66d74436ca7d0e66e)). This feature has shown significant performance improvements, as detailed in the following articles:

[Phoronix Article on FUSE Passthrough](https://www.phoronix.com/news/FUSE-Passthrough-In-6.9-Next)<br>

FUSE Passthrough allows performing read and write (also via memory maps) on a backing file without incurring the overhead of roundtrips to userspace.

![passhthrough feature](/docs/images/passthrough01.png)

Additionally, the `go-fuse` package, which Stargz-Snapshotter depends on, has also added support for this passthrough feature:

[go-fuse Commit 1](https://github.com/hanwen/go-fuse/commit/e0641a46c6cca7e5370fc135f78caf7cb7fc3aa8#diff-f830ac3db25844bf71102b09e4e02f7213e9cdb577b32745979d61d775462bd3R157)<br>
[go-fuse Commit 2](https://github.com/hanwen/go-fuse/commit/e0a0b09ae8287249c38033a27fd69a3593c7e235#diff-1521152f1fc3600273bda897c669523dc1e9fc9cbe24046838f043a8040f0d67R749)<br>
[go-fuse Commit 3](https://github.com/hanwen/go-fuse/commit/1a7d98b0360f945fca50ac79905332b7106c049f)

When a user-defined file implements the `FilePassthroughFder` interface, `go-fuse` will attempt to register the file `fd` from the file with the kernel.

# Configuration

To enable FUSE passthrough mode, first verify that your host's kernel supports this feature. You can check this by running the following command:

```bash
$ cat /boot/config-$(uname -r) | grep "CONFIG_FUSE_PASSTHROUGH=y"
CONFIG_FUSE_PASSTHROUGH=y
```

Once you have confirmed kernel support, you need to enable passthrough mode in your `config.toml` file with the following configuration:

```toml
[fuse]
passthrough = true
```

After updating the configuration, specify the `config.toml` file when starting `containerd-stargz-grpc` and restart the service:

```bash
$ containerd-stargz-grpc -config config.toml
```

# Important Considerations

When passthrough mode is enabled, the following configuration is applied by default, even if it is set to false in the configuration file:

```toml
[directory_cache]
direct = true
```

This is because, in passthrough mode, read operations after opening a file are handled directly by the kernel.
