# Creating smaller eStargz images

The following flags of `ctr-remote i convert` and `ctr-remote i optimize` allow users optionally creating smaller eStargz images.

- `--estargz-external-toc`: Separate TOC JSON into another image (called "TOC image"). The result eStargz doesn't contain TOC so we can expect a smaller size than normal eStargz.

- `--estargz-min-chunk-size`: The minimal number of bytes of data must be written in one gzip stream. If it's > 0, multiple files and chunks can be written into one gzip stream. Smaller number of gzip header and smaller size of the result blob can be expected. `--estargz-min-chunk-size=0` produces normal eStargz.

## `--estargz-external-toc` usage

convert:

```console
# ctr-remote i pull ghcr.io/stargz-containers/ubuntu:22.04
# ctr-remote i convert --oci --estargz --estargz-external-toc ghcr.io/stargz-containers/ubuntu:22.04 registry2:5000/ubuntu:22.04-ex
```

Layers in eStargz (`registry2:5000/ubuntu:22.04-ex`) don't contain TOC JSON.
TOC image (`registry2:5000/ubuntu:22.04-ex-esgztoc`) contains TOC of all layers of the eStargz image.
Suffix `-esgztoc` is automatically added to the image name by `ctr-remote`.

Then push eStargz(`registry2:5000/ubuntu:22.04-ex`) and TOC image(`registry2:5000/ubuntu:22.04-ex-esgztoc`) to the same registry:

```console
# ctr-remote i push --plain-http registry2:5000/ubuntu:22.04-ex
# ctr-remote i push --plain-http registry2:5000/ubuntu:22.04-ex-esgztoc
```

Pull it lazily:

```console
# ctr-remote i rpull --plain-http registry2:5000/ubuntu:22.04-ex
fetching sha256:14fb0ea2... application/vnd.oci.image.index.v1+json
fetching sha256:24471b45... application/vnd.oci.image.manifest.v1+json
fetching sha256:d2e4737e... application/vnd.oci.image.config.v1+json
# mount | grep "stargz on"
stargz on /var/lib/containerd-stargz-grpc/snapshotter/snapshots/1/fs type fuse.rawBridge (rw,nodev,relatime,user_id=0,group_id=0,allow_other)
```

Stargz Snapshotter automatically refers to the TOC image on the same registry.

### optional `--estargz-keep-diff-id` flag for conversion without changing layer diffID

`ctr-remote i convert` supports optional flag `--estargz-keep-diff-id` specified with `--estargz-external-toc`.
This converts an image to eStargz without changing the diffID (uncompressed digest) so even eStargz-agnostic gzip decompressor (e.g. gunzip) can restore the original tar blob.

```console
# ctr-remote i pull ghcr.io/stargz-containers/ubuntu:22.04
# ctr-remote i convert --oci --estargz --estargz-external-toc --estargz-keep-diff-id ghcr.io/stargz-containers/ubuntu:22.04 registry2:5000/ubuntu:22.04-ex-keepdiff
# ctr-remote i push --plain-http registry2:5000/ubuntu:22.04-ex-keepdiff
# ctr-remote i push --plain-http registry2:5000/ubuntu:22.04-ex-keepdiff-esgztoc
# crane --insecure blob registry2:5000/ubuntu:22.04-ex-keepdiff@sha256:2dc39ba059dcd42ade30aae30147b5692777ba9ff0779a62ad93a74de02e3e1f | jq -r '.rootfs.diff_ids[]'
sha256:7f5cbd8cc787c8d628630756bcc7240e6c96b876c2882e6fc980a8b60cdfa274
# crane blob ghcr.io/stargz-containers/ubuntu:22.04@sha256:2dc39ba059dcd42ade30aae30147b5692777ba9ff0779a62ad93a74de02e3e1f | jq -r '.rootfs.diff_ids[]'
sha256:7f5cbd8cc787c8d628630756bcc7240e6c96b876c2882e6fc980a8b60cdfa274
```

## `--estargz-min-chunk-size` usage

conversion:

```console
# ctr-remote i pull ghcr.io/stargz-containers/ubuntu:22.04
# ctr-remote i convert --oci --estargz --estargz-min-chunk-size=50000 ghcr.io/stargz-containers/ubuntu:22.04 registry2:5000/ubuntu:22.04-chunk50000
# ctr-remote i push --plain-http registry2:5000/ubuntu:22.04-chunk50000
```

Pull it lazily:

```console
# ctr-remote i rpull --plain-http registry2:5000/ubuntu:22.04-chunk50000
fetching sha256:5d1409a2... application/vnd.oci.image.index.v1+json
fetching sha256:859e2b50... application/vnd.oci.image.manifest.v1+json
fetching sha256:c07a44b9... application/vnd.oci.image.config.v1+json
# mount | grep "stargz on"
stargz on /var/lib/containerd-stargz-grpc/snapshotter/snapshots/1/fs type fuse.rawBridge (rw,nodev,relatime,user_id=0,group_id=0,allow_other)
```

> NOTE: This flag creates an eStargz image with newly-added `innerOffset` funtionality of eStargz. Stargz Snapshotter < v0.13.0 cannot perform lazy pulling for the images created with this flag.
