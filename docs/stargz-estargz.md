# Stargz and eStargz Archive Format

This doc describes image layer archive formats *stargz* and *eStargz* used in this project for enabling *lazy image pull*.

Currently, [OCI](https://github.com/opencontainers/image-spec/)/[Docker](https://github.com/moby/moby/blob/master/image/spec/v1.2.md) Image Spec uses tar.gz (or tar) for archiving layers but these formats don't suit to lazy pull.
Stargz and eStargz solve this problem and enable lazy pull with stargz snapshotter.
They are still compatible with legacy tar.gz so runtimes can use these formats as legacy tar.gz image layers.

![the overview of stargz and eStargz](/docs/images/stargz-estargz.png)

## Stargz archive format

When pulling an image lazily, the runtime doesn’t download the entire contents but fetches necessary data chunks *on-demand*.
For archiving this, runtimes need to *selectively* fetch and extract files in the image layers.
However current OCI Image Spec uses tar and tar.gz archive formats for archiving layers, which don't suit to this use-case because of the following reasons,

1. We need to scan the whole archive even for finding and extracting single file entry.
2. If the archive is compressed by gzip, this is no longer seekable.

[Stargz](https://github.com/google/crfs) is an archive format proposed by Google for solving these issues and enables lazy pull.
Initially, this format was [discussed in Go community](https://github.com/golang/go/issues/30829) for optimizing their building system.
Stargz stands for *seekable tar.gz* and it's file entries can be selectively extracted.
This archive is still valid tar.gz so stargz archived layers are still usable as legacy tar.gz layers for legacy runtimes.

### The structure of stargz

In stargz archive, each tar entries of regular files are separately compressed by gzip.
A stargz archive is the concatenation of these gzip members, which is still valid gzip.
For large files, the contents are chunked into several gzip members.
Entries which only contains file metadata (e.g. symlink) are put in a gzip member together with the neighbouring entries.

An index file entry called *TOC* is contained following the gzip entries.
This file is a JSON file which records the size and offset of chunks corresponding to each file's contents in the stargz archive as well as the each file's metadata (name, file type, owners, etc).
[All keys available in TOC are defined in the CRFS project repo](https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L214-L218).

At the tail of stargz, a *footer* is appended.
This is an empty gzip entry whose header (extra field) contains the offset of TOC in the archive.
The footer is defined to be 47 bytes (1 byte = 8 bits in gzip).
[The bit structure of the footer is defined in the CRFS project repo](https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L38-L48).

Runtimes can first read the tail 47 bytes footer of the archive and get the offset of TOC.
Each file's metadata is recorded in the TOC so runtimes don't need to extract other parts of the stargz archive as long as it only uses file metadata.
If runtime needs to get regular file's contents, it gets size and offset of the chunk corresponded to the file contents in the TOC and extract that range without scanning the whole archive.
By combining this with HTTP Range Request supported by [OCI](https://github.com/opencontainers/distribution-spec/blob/master/spec.md#fetch-blob-part)/[Docker](https://docs.docker.com/registry/spec/api/#fetch-blob-part) registry API, runtimes can selectively download file entries from registries

For more detailed structure, refer to the [CRFS project repo](https://github.com/google/crfs).

## eStargz archive format

Lazy pull costs extra time for reading files which induces remotely fetching file contents.
The eStargz archive format is an extended version of stargz for solving this problem by the ability to indicate *the likely accessed files* during runtime.
Runtimes can prefetch these files, which hopefully increases cache hit ratio and mitigates the read overhead.
This format is backwards compatible to stargz so can be used as legacy tar.gz layers by legacy runtimes.

### The structure of eStargz

The structure of eStargz is same as stargz archive except it can indicate the information about likely accessed files as the *order* of file entries, with some [*landmark* file entries](https://github.com/containerd/stargz-snapshotter/blob/28af649b55ac39efc547b2e7f14f81a33a8212e1/stargz/fs.go#L93-L99).
File entries in eStargz are grouped into the following groups,

- files *likely accessed* by containers during runtime, and
- files not likely accessed

An eStargz archive is made with two separated areas corresponding to these groups.
A special file entry *prefetch landmark* (named `.prefetch.landmark`) indicates the border between these two areas.
That is, entries stored in the range between the top and the prefetch landmark are likely accessed during runtime.
In the range of likely accessed files, entries are sorted by a possible accessed order.
If there are no files possibly accessed in the archive, another type of landmark file *no-prefetch landmark* (named `.no.prefetch.landmark`) is contained in the archive.
These landmark files can be any file type but the content offset must be recorded in the TOC of the stargz archive.

On container startup, runtimes can prefetch the range where likely accessed files are contained, which can mitigate the read overhead.
If runtimes find no-prefetch landmark, they don't need to prefetch anything.

### eStargz and workload-based image optimization in Stargz Snapshotter

Stargz Snapshotter makes use of eStargz for *workload-based* optimization for mitigating overhead of reading files.

Generally, container images are built with purpose and the workloads are determined at the build.
In many cases, a workload is defined in the Dockerfile using some parameters including entrypoint command, environment variables and user.

Stargz snapshotter provides an image converter command `ctr-remote images optimize`.
This leverages eStargz archive format and mitigates reading performance for files that are *likely accessed* in the workload defined in the Dockerfile.

When converting the image, this command runs the specified workload in a sandboxed environment and profiles all file accesses.
This command regards all accessed files as likely accessed also in production.
Then it constructs eStargz archive by

- locating accessed files from top of the archive, with sorting them by the accessed order,
- putting prefetch landmark file entry at the end of this range, and
- locating all other files (not accessed files) after the prefetch landmark.

Before running container, stargz snapshotter prefetches and pre-caches this range by a single HTTP Range Request.
This can increase the cache hit rate for the specified workload and mitigates runtime overheads.
