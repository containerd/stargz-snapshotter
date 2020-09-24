# Content Verification in eStargz

The goal of the content verification in eStargz is to ensure the downloaded metadata and contents of all files are the expected ones, based on the calculated OCI [_digests_](https://github.com/opencontainers/image-spec/blob/v1.0.1/descriptor.md#digests).
The verification of other components in the image including image manifests is out-of-scope.
On the verification step of an eStargz layer, we assume that the [_manifest_](https://github.com/opencontainers/image-spec/blob/v1.0.1/manifest.md) that references this eStargz layer is verified in containerd in advance (using digest tag or [`Docker-Content-Digest` header](https://docs.docker.com/registry/spec/api/#digest-header), etc).

![the overview of the verification](/docs/images/verification.png)

## Verifiable eStargz

For a layer that isn't lazily pulled (i.e. traditional tar.gz layer), it can be verified by recalculating the digest and compare it with the one written in the layer [_descriptor_](https://github.com/opencontainers/image-spec/blob/v1.0.1/descriptor.md) referencing that layer in the verified manifest.
However, an eStargz layer is **lazily** pulled from the registry in file (or chunk if that file is large) granularity.
So it's not possible to recalculate and verify the digest of the entire layer on mount.
Assuming that the manifest referencing the eStargz layer has already been verified, we verify that eStargz layer as the following.

When stargz snapshotter lazily pulls an eStargz (or stargz) layer, the following components will be fetched from the registry.

- TOC (a set of metadata of all files contained in the layer)
- chunks of regular file contents

As mentioned in [stargz and eStargz documentation](/docs/stargz-estargz.md), eStargz (and stargz) contains an index file called _TOC_.
Not only offset information of file entries, it [contains metadata (name, type, mode, etc.) of all files contained in the layer blob](https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L214-L218).
On mount the layer, filesystem fetches the TOC from the registry.
For making the TOC verifiable using the manifest, we define an [_annotation_](https://github.com/opencontainers/image-spec/blob/v1.0.1/descriptor.md#properties) `containerd.io/snapshot/stargz/toc.digest`.
The value of this annotation is the digest of the TOC and this annotation must be contained in descriptors that references this eStargz layer in the manifest.
Using this annotation, filesystem can verify the TOC by recalculating the digest and compare it to the one written in the verified manifest.

Each file's metadata (name, type, mode, etc.) is formed as a [_TOCEntry_](https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L109-L191) in the TOC.
TOCEntry is also created for each chunk of regular file content.
For making each chunk verifiable using the manifest, eStargz extends the TOCEntry definition with [an optional field `chunkDigest`](https://github.com/containerd/stargz-snapshotter/blob/b53e8fe8d37751753bc623b037729b6a6d9c1122/stargz/verify/verify.go#L56-L64).
`chunkDigest` is a field to contain the digest of each chunk.
As mentioned in the above, the TOC is verifiable using the manifest with the special annotation.
So using `chunkDigest` fields, filesystem can verify each chunk by recalculating the digest and compare it to the one written in the verified TOC.

If the following conditions meet, an eStargz layer is verifiable.

- the digest of the TOC is contained in the annotation(`containerd.io/snapshot/stargz/toc.digest`) of descriptors that references this layer, and
- `chunkDigest` fields of all chunks in the TOC is filled with the digests of their contents.

`ctr-remote images optimize` command in this project creates the verifiable eStargz image by default.

## Content verification in Stargz Snapshotter

Stargz Snapshotter verifies eStargz layers leveraging the above extensions.
However, as mentioned in the above, the verification of other image component including the manifests is out-of-scope of the snapshotter.
So when this snapshotter mounts an eStargz layer, the manifest that references this layer must be verified in the containerd in advance and the TOC's digest written in the manifest (as an annotation `containerd.io/snapshot/stargz/toc.digest`) must be passed down to this snapshotter.

If the manifest doesn't contain `containerd.io/snapshot/stargz/toc.digest` annotation, verification can't be done for that layer so stargz snapshotter reports an error and doesn't mount it.
You can bypass this check only if both of the following conditions meet.

- `allow_no_verification = true` is specified in `config.toml` of stargz snapshotter, and
- the content descriptor of this layer has an annotation `containerd.io/snapshot/remote/stargz.skipverify` (the value will be ignored).

The other way is to disable verification completely by setting `disable_verification = true` in `config.toml` of stargz snapshotter.

On mounting a layer, stargz snapshotter fetches this layer's TOC from the registry.
Then it verifies the TOC by recaluculating the digest and comparing it with the one passed from containerd (written in the manifest).
If the TOC is successfully verified, then the snapshotter mounts this layer using the metadata stored in the TOC.
During runtime of the container, this snapshotter fetches chunks of regular files lazily.
Before providing a chunk to the filesystem user, snapshotter recalculates the digest and checks it matches the one contained in the corresponding TOCEntry in the TOC.
