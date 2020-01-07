# CRFS: Container Registry Filesystem

Discussion: https://github.com/golang/go/issues/30829

## Overview

**CRFS** is a read-only FUSE filesystem that lets you mount a
container image, served directly from a container registry (such as
[gcr.io](https://gcr.io/)), without pulling it all locally first.

## Background

Starting a container should be fast. Currently, however, starting a
container in many environments requires doing a `pull` operation from
a container registry to read the entire container image from the
registry and write the entire container image to the local machine's
disk. It's pretty silly (and wasteful) that a read operation becomes a
write operation. For small containers, this problem is rarely noticed.
For larger containers, though, the pull operation quickly becomes the
slowest part of launching a container, especially on a cold node.
Contrast this with launching a VM on major cloud providers: even with
a VM image that's hundreds of gigabytes, the VM boots in seconds.
That's because the hypervisors' block devices are reading from the
network on demand. The cloud providers all have great internal
networks. Why aren't we using those great internal networks to read
our container images on demand?

## Why does Go want this?

Go's continuous build system tests Go on [many operating systems and
architectures](https://build.golang.org/), using a mix of containers
(mostly for Linux) and VMs (for other operating systems). We
prioritize fast builds, targeting 5 minute turnaround for pre-submit
tests when testing new changes. For isolation and other reasons, we
run all our containers in a single-use fresh VMs. Generally our
containers do start quickly, but some of our containers are very large
and take a long time to start. To work around that, we've automated
the creation of VM images where our heavy containers are pre-pulled.
This is all a silly workaround. It'd be much better if we could just
read the bytes over the network from the right place, without the all
the hoops.

## Tar files

One reason that reading the bytes directly from the source on demand
is somewhat non-trivial is that container images are, somewhat
regrettably, represented by *tar.gz* files, and tar files are
unindexed, and gzip streams are not seekable. This means that trying
to read 1KB out of a file named `/var/lib/foo/data` still involves
pulling hundreds of gigabytes to uncompress the stream, to decode the
entire tar file until you find the entry you're looking for. You can't
look it up by its path name.

## Introducing Stargz

Fortunately, we can fix the fact that *tar.gz* files are unindexed and
unseekable, while still making the file a valid *tar.gz* file by
taking advantage of the fact that two gzip streams can be concatenated
and still be a valid gzip stream. So you can just make a tar file
where each tar entry is its own gzip stream.

We introduce a format, **Stargz**, a **S**eekable
**tar.gz** format that's still a valid tar.gz file for everything else
that's unaware of these details.

In summary:

* That traditional `*.tar.gz` format is: `Gzip(TarF(file1) + TarF(file2) + TarF(file3) + TarFooter))`
* Stargz's format is: `Gzip(TarF(file1)) + Gzip(TarF(file2)) + Gzip(TarF(file3_chunk1)) + Gzip(F(file3_chunk2)) + Gzip(F(index of earlier files in magic file), TarFooter)`, where the trailing ZIP-like index contains offsets for each file/chunk's GZIP header in the overall **stargz** file.

This makes images a few percent larger (due to more gzip headers and
loss of compression context between files), but it's plenty
acceptable.

## Converting images

If you're using `docker push` to push to a registry, you can't use
CRFS to mount the image. Maybe one day `docker push` will push
*stargz* files (or something with similar properties) by default, but
not yet. So for now we need to convert the storage image layers from
*tar.gz* into *stargz*. There is a tool that does that. **TODO: examples**

## Operation

When mounting an image, the FUSE filesystem makes a couple Docker
Registry HTTP API requests to the container registry to get the
metadata for the container and all its layers.

It then does HTTP Range requests to read just the **stargz** index out
of the end of each of the layers. The index is stored similar to how
the ZIP format's TOC is stored, storing a pointer to the index at the
very end of the file. Generally it takes 1 HTTP request to read the
index, but no more than 2. In any case, we're assuming a fast network
(GCE VMs to gcr.io, or similar) with low latency to the container
registry. Each layer needs these 1 or 2 HTTP requests, but they can
all be done in parallel.

From that, we keep the index in memory, so `readdir`, `stat`, and
friends are all served from memory. For reading data, the index
contains the offset of each file's `GZIP(TAR(file data))` range of the
overall *stargz* file. To make it possible to efficiently read a small
amount of data from large files, there can actually be multiple
**stargz** index entries for large files. (e.g. a new gzip stream
every 16MB of a large file).

## Union/overlay filesystems

CRFS can do the aufs/overlay2-ish unification of multiple read-only
*stargz* layers, but it will stop short of trying to unify a writable
filesystem layer atop. For that, you can just use the traditional
Linux filesystems.

## Using with Docker, without modifying Docker

Ideally container runtimes would support something like this whole
scheme natively, but in the meantime a workaround is that when
converting an image into *stargz* format, the converter tool can also
produce an image variant that only has metadata (environment,
entrypoints, etc) and no file contents. Then you can bind mount in the
contents from the CRFS FUSE filesystem.

That is, the convert tool can do:

**Input**: `gcr.io/your-proj/container:v2`

**Output**: `gcr.io/your-proj/container:v2meta` + `gcr.io/your-proj/container:v2stargz`

What you actually run on Docker or Kubernetes then is the `v2meta`
version, so your container host's `docker pull` or equivalent only
pulls a few KB. The gigabytes of remaining data is read lazily via
CRFS from the `v2stargz` layer directly from the container registry.

## Status

WIP. Enough parts are implemented & tested for me to realize this
isn't crazy. I'm publishing this document first for discussion while I
finish things up. Maybe somebody will point me to an existing
implementation, which would be great.

## Discussion

See https://github.com/golang/go/issues/30829
