# eStargz: Standard-Compatible Extensions to Tar.gz Layers for Lazy Pulling Container Images

This doc describes the extension to image layers for enabling *lazy image pulling*.
The extended layer format is called *eStargz* in this project.
eStargz is backward-compatible to tar.gz layers used in the current [OCI](https://github.com/opencontainers/image-spec/)/[Docker](https://github.com/moby/moby/blob/master/image/spec/v1.2.md) Image Specs so eStargz-formatted images can be pushed to and lazily pulled from standard registries.
Furthermore, they can run even on extension-agnostic runtimes (e.g. Docker).

This extension is based on stargz (stands for *seekable tar.gz*) proposed by [Google CRFS](https://github.com/google/crfs) project (initially [discussed in Go community](https://github.com/golang/go/issues/30829)).
eStargz is an extended-version of stargz and comes with additional features including chunk-level verification and runtime performance optimization.

Notational convention follows [OCI Image Spec](https://github.com/opencontainers/image-spec/blob/v1.0.1/spec.md#notational-conventions).

## Overview

When lazily pulling an image from the registry, necessary chunks of its layers are fetched *on-demand* during running the container, instead of downloading the entire contents of that image at once.
For achieving this, runtimes need to *selectively* fetch and extract files contents in the layer.
However, current OCI/Docker Image Spec uses tar (optionally with compression) for archiving layers, which doesn't suit to this use-case because of the following reasons,

1. The entire archive needs to be scanned even for finding and extracting a single file.
2. If the archive is compressed by gzip, this is no longer seekable.
3. File entries in the archive cannot be verified separately (In Docker/OCI specs, verification is done for *the entire contents of the layer*, not per entry).

eStargz is a tar.gz-compatible archive format which solves these issues and enables lazy pulling.
Each file (or chunk for large files) in eStargz can be extracted selectively and verified separately.
Additionally, eStargz has a feature called *prioritized files* for mitigating runtime performance drawbacks caused by on-demand fetching of each file/chunk.
This format is compatible to tar.gz so eStargz layers are storable to container registries, lazily-pullable from container registries and still runnable even on eStargz-agnostic runtimes.

This doc defines the basic structure of eStargz layer that has the above features.
For details about content verfication in eStargz, please refer to [Content Verification in eStargz](/docs/verification.md).

## The structure

![The structure of eStargz](/docs/images/estargz-structure.png)

In eStargz archive, each non-empty regular file is separately compressed by gzip.
This structure is inherited from [stargz](https://github.com/google/crfs).

The gzip headers MUST locate at the following locations.

- The top of the tar archive
- The top of the payload of each non-empty regular file entry except *TOC*
- The top of *TOC* tar header
- The top of *footer* (described in the later section)

The gzip headers MAY locate at the following locations.

- The end of the payload of each non-empty regular file entry
- Arbitrary location within the payload of non-empty regular file entry

The gzip header locations described in the second item MAY be used for chunking large regular files into several gzip members.
Each chunked member is called *chunk* in this doc.
An eStargz archive is the concatenation of these gzip members, which is a still valid gzip.

## TOC, TOCEntries and Footer

### TOC and TOCEntries

A regular file entry called *TOC* MUST be contained as the last tar entry in the archive.
TOC MUST be a JSON file and MUST be named `stargz.index.json`.

TOC records all file's metadata (e.g. name, file type, owners, offset etc) in the tar archive, except TOC itself.
The TOC is defined as the following.

- **`version`** *int*

   This REQUIRED property contains the version of the TOC. This value MUST be `1`.

- **`entries`** *array of objects*

   Each item in the array MUST be a TOCEntry.
   This property MUST contain TOCEntries that reflect all tar entries and chunks, except `stargz.index.json`.

The TOCEntry is defined as the following.
If the information written in TOCEntry differs from the corresponding tar entry, TOCEntry SHOULD be respected.
TOCEntries fields other than `chunkDigest` are inherited from [stargz](https://github.com/google/crfs).

- **`name`** *string*

  This REQUIRED property contains the name of the tar entry.
  This MUST be the complete path stored in the tar file.

- **`type`** *string*

  This REQUIRED property contains the type of the tar entry.
  This MUST be either of the following.
  - `dir`: directory
  - `reg`: regular file
  - `symlink`: symbolic link
  - `hardlink`: hard link
  - `char`: character device
  - `block`: block device
  - `fifo`: fifo
  - `chunk`: a chunk of regular file data
  As described in the above section, a regular file can be divided into several chunks.
  Corresponding to the first chunk of that file, TOCEntry typed `reg` MUST be contained.
  Corresponding to the chunks after 2nd, TOCEntries typed `chunk` MUST be contained.
  `chunk`-typed TOCEntry must set offset, chunkOffset and chunkSize properties.

- **`size`** *uint64*

  This OPTIONAL property contains the uncompressed size of the regular file tar entry.

- **`modtime`** *string*

  This OPTIONAL property contains the modification time of the tar entry.
  Empty means zero or unknown.
  Otherwize, the value is in UTC RFC3339 format.

- **`linkName`** *string*

  This OPTIONAL property contains the link target of `symlink` and `hardlink`.

- **`mode`** *int64*

  This OPTIONAL property contains the permission and mode bits.

- **`uid`** *uint*

  This OPTIONAL property contains the user ID of the owner of this file.

- **`gid`** *uint*

  This OPTIONAL property contains the group ID of the owner of this file.

- **`userName`** *string*

  This OPTIONAL property contains the username of the owner.

- **`groupName`** *string*

  This OPTIONAL property contains the groupname of the owner.

- **`offset`** *int64*

  This OPTIONAL property contains the offset of the gzip header of the regular file or chunk in the archive.

- **`devMajor`** *int*

  This OPTIONAL property contains the major device number for character and block device files.

- **`devMinor`** *int*

  This OPTIONAL property contains the minor device number for character and block device files.

- **`xattrs`** *string-bytes map*

  This OPTIONAL property contains the extended attribute for the tar entry.

- **`digest`** *string*

  This OPTIONAL property contains the OCI [Digest](https://github.com/opencontainers/image-spec/blob/v1.0.1/descriptor.md#digests) of the regular file contents.
  TOCEntries of non-empty `reg` file MUST set this property.

- **`chunkOffset`** *int64*

  This OPTIONAL property contains the offset of this chunk in the regular file payload.
  Note that this is the offset of this chunk in the decompressed file content.
  TOCEntries of `chunk` type MUST set this property.

- **`chunkSize`** *int64*

  This OPTIONAL property contains the decompressed size of this chunk.
  The last `chunk` in a `reg` file or `reg` file that isn't chunked MUST set this property to zero.
  Other `reg` and `chunk` MUST set this property.

- **`chunkDigest`** *string*

  This OPTIONAL property contains an OCI [Digest](https://github.com/opencontainers/image-spec/blob/v1.0.1/descriptor.md#digests) of this chunk.
  TOCEntries of non-empty `reg` and `chunk` MUST set this property.
  This MAY be used for verifying the data of this entry in the way described in [Content Verification in eStargz](/docs/verification.md).

### Footer

At the end of the archive, a *footer* MUST be appended.
This MUST be an empty gzip member ([RFC1952](https://tools.ietf.org/html/rfc1952)) whose [Extra field](https://tools.ietf.org/html/rfc1952#section-2.3.1.1) contains the offset of TOC in the archive.
The footer MUST be the following 51 bytes (1 byte = 8 bits in gzip).

```
- 10 bytes  gzip header
- 2  bytes  XLEN (length of Extra field) = 26 (4 bytes header + 16 hex digits + len("STARGZ"))
- 2  bytes  Extra: SI1 = 'S', SI2 = 'G'
- 2  bytes  Extra: LEN = 22 (16 hex digits + len("STARGZ"))
- 22 bytes  Extra: subfield = fmt.Sprintf("%016xSTARGZ", offsetOfTOC)
- 5  bytes  flate header: BFINAL = 1(last block), BTYPE = 0(non-compressed block), LEN = 0
- 8  bytes  gzip footer
(End of eStargz)
```

Runtimes MAY first read and parse the footer of the archive to get the offset of TOC.
Each file's metadata is recorded in the TOC so runtimes don't need to extract other parts of the archive as long as it only uses file metadata.
If runtime needs to get a regular file's content, it MAY get size and offset information of that content from the TOC and MAY extract that range without scanning the whole archive.
By combining this with HTTP Range Request supported by [OCI Distribution Spec](https://github.com/opencontainers/distribution-spec/blob/main/detail.md#fetch-blob-part) and [Docker Registry API](https://docs.docker.com/registry/spec/api/#fetch-blob-part), runtimes can selectively download file entries from registries

### Notes on compatibility with stargz

eStargz is designed aiming to the compatibility with tar.gz.
For achieving this, eStargz's footer structure is incompatible to [stargz's one](https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L36-L49).
eStargz adds SI1, SI2 and LEN fields to the footer for making it compliant to [Extra field definition in RFC1952](https://tools.ietf.org/html/rfc1952#section-2.3.1.1).
TOC, TOCEntry and the position of gzip headers are still compatible with stargz.

## Prioritized Files and Landmark Files

![Prioritized files and landmark files](/docs/images/estargz-landmark.png)

Lazy pulling costs extra time for reading files which induces remotely fetching file contents.
The eStargz archive mitigates this problem with the ability to indicate the likely accessed files called *prioritized files*.
Runtimes can leverage this information (e.g. for prefetching prioritized files) for increasing cache hit ratio and mitigating the read overhead (example usage of this information in Stargz Snapshotter is described in the later section).

eStargz indicates the information about prioritized files as the *order* of file entries, with some [*landmark* file entries](https://github.com/containerd/stargz-snapshotter/blob/28af649b55ac39efc547b2e7f14f81a33a8212e1/stargz/fs.go#L93-L99).

File entries in eStargz are grouped into the following groups,

- A. files *likely accessed* by containers during runtime (i.e. prioritized files), and
- B. files not likely accessed

If there are no files belong to A, a landmark file *no-prefetch landmark* MUST be contained in the archive.
If there are files belong to A, an eStargz archive MUST be made with two separated areas corresponding to these groups and a landmark file *prefetch landmark* MUST be containerd at the border between these two areas.
That is, entries stored in the range between the top and the prefetch landmark are likely accessed during runtime.

Both of landmark files MUST be regular file entries with 4 bits contents 0xf.
Prefetch landmark MUST be registered to TOC as a TOCEntry named `.prefetch.landmark` and no-prefetch landmark MUST be registered as a TOCEntry named `.no.prefetch.landmark`.

On container startup, the runtime SHOULD prefetch the range where prioritized files are contained.
When the runtime finds no-prefetch landmark, it SHOULD NOT prefetch anything.

## Example use-case of prioritized files: workload-based image optimization in Stargz Snapshotter

Stargz Snapshotter makes use of eStargz's prioritized files for *workload-based* optimization for mitigating overhead of reading files.

Generally, container images are built with purpose and the workloads are determined at the build.
In many cases, a workload is defined in the Dockerfile using some parameters including entrypoint command, environment variables and user.

Stargz snapshotter provides an image converter command `ctr-remote images optimize`.
This leverages eStargz archive format and mitigates reading performance for files that are *likely accessed* in the workload defined in the Dockerfile.

When converting the image, this command runs the specified workload in a sandboxed environment and profiles all file accesses.
This command regards all accessed files as likely accessed also in production (i.e. prioritized files).
Then it constructs eStargz archive by

- locating accessed files from top of the archive, with sorting them by the accessed order,
- putting prefetch landmark file entry at the end of this range, and
- locating all other files (not accessed files) after the prefetch landmark.

Before running the container, stargz snapshotter prefetches and pre-caches the range where prioritized files are contained, by a single HTTP Range Request.
This can increase the cache hit rate for the specified workload and can mitigate runtime overheads.

## Example of TOC

You can inspect TOC JSON generated by `ctr-remote` converter like the following:

```
ctr-remote image pull ghcr.io/stargz-containers/alpine:3.10.2-org
ctr-remote image optimize --oci ghcr.io/stargz-containers/alpine:3.10.2-org alpine:3.10.2-esgz
ctr-remote content get sha256:42d069d45aac902b9ad47365613f517bbcfb567674bd78a36fbfe7c2e1ca4d75 \
  | tar xzOf -  stargz.index.json | jq
```

Then you will get the TOC JSON something like:

```json
{
  "version": 1,
  "entries": [
    {
      "name": "bin/",
      "type": "dir",
      "modtime": "2019-08-20T10:30:43Z",
      "mode": 16877,
      "NumLink": 0
    },
    {
      "name": "bin/busybox",
      "type": "reg",
      "size": 833104,
      "modtime": "2019-06-12T17:52:45Z",
      "mode": 33261,
      "offset": 126,
      "NumLink": 0,
      "digest": "sha256:8b7c559b8cccca0d30d01bc4b5dc944766208a53d18a03aa8afe97252207521f",
      "chunkDigest": "sha256:8b7c559b8cccca0d30d01bc4b5dc944766208a53d18a03aa8afe97252207521f"
    },
    {
      "name": "lib/",
      "type": "dir",
      "modtime": "2019-08-20T10:30:43Z",
      "mode": 16877,
      "NumLink": 0
    },
    {
      "name": "lib/ld-musl-x86_64.so.1",
      "type": "reg",
      "size": 580144,
      "modtime": "2019-08-07T07:15:30Z",
      "mode": 33261,
      "offset": 512427,
      "NumLink": 0,
      "digest": "sha256:45c6ee3bd1862697eab8058ec0e462f5a760927331c709d7d233da8ffee40e9e",
      "chunkDigest": "sha256:45c6ee3bd1862697eab8058ec0e462f5a760927331c709d7d233da8ffee40e9e"
    },
    {
      "name": ".prefetch.landmark",
      "type": "reg",
      "size": 1,
      "offset": 886633,
      "NumLink": 0,
      "digest": "sha256:dc0e9c3658a1a3ed1ec94274d8b19925c93e1abb7ddba294923ad9bde30f8cb8",
      "chunkDigest": "sha256:dc0e9c3658a1a3ed1ec94274d8b19925c93e1abb7ddba294923ad9bde30f8cb8"
    },
... (omit) ...
```
