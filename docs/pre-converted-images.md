# Trying pre-converted images

We have several pre-converted stargz images on Github Container Registry, mainly for benchmarking purpose. This doc lists these images in a table format. You can try them on your machine with our snapshotter. Please refer to README for the procedure.

Please do not use them in production. You always can build your eStargz images optimized for your workload, using [`ctr-remote` command](/docs/ctr-remote.md).

## Pre-converted images

This section contains a table of pre-converted images which can be used for benchmarking, testing, etc.

We have pre-converted images on GitHub Container Registry. Images are stored under the repository `ghcr.io/stargz-containers`.

Additionally, image names listed in `Image Name` contain the following suffixes based on the type of the image.

- `org`: Legacy image copied from `docker.io/library` without optimization. Layers are normal tarballs.
- `esgz`: eStargz-formatted version of the `org` images. `ctr-remote images optimize` command is used for the optimization.

`Optimized Workload` column describes workloads used for building `esgz` images. We optimized these images for benchmarking which is based on [HelloBench](https://github.com/Tintri/hello-bench) so we specified "hello-world"-like workloads for the command. See [benchmarking script](/script/benchmark/hello-bench/src/hello.py) for the exact command option specified for `ctr-remote images optimize`. 

|Image Name|Optimized Workload|
---|---
|`ghcr.io/stargz-containers/alpine:3.10.2-org`|Executing `echo hello` on the shell|
|`ghcr.io/stargz-containers/alpine:3.10.2-esgz`|Executing `echo hello` on the shell|
|`ghcr.io/stargz-containers/drupal:8.7.6-org`|Code execution until up and ready message (`apache2 -D FOREGROUND`) is printed|
|`ghcr.io/stargz-containers/drupal:8.7.6-esgz`|Code execution until up and ready message (`apache2 -D FOREGROUND`) is printed|
|`ghcr.io/stargz-containers/fedora:30-org`|Executing `echo hello` on the shell|
|`ghcr.io/stargz-containers/fedora:30-esgz`|Executing `echo hello` on the shell|
|`ghcr.io/stargz-containers/gcc:10.2.0-org`|Compiling and executing a program which prints `hello`|
|`ghcr.io/stargz-containers/gcc:10.2.0-esgz`|Compiling and executing a program which prints `hello`|
|`ghcr.io/stargz-containers/golang:1.12.9-org`|Compiling and executing a program which prints `hello`|
|`ghcr.io/stargz-containers/golang:1.12.9-esgz`|Compiling and executing a program which prints `hello`|
|`ghcr.io/stargz-containers/jenkins:2.60.3-org`|Code execution until up and ready message (`Jenkins is fully up and running`) is printed|
|`ghcr.io/stargz-containers/jenkins:2.60.3-esgz`|Code execution until up and ready message (`Jenkins is fully up and running`) is printed|
|`ghcr.io/stargz-containers/jruby:9.2.8.0-org`|Printing `hello`|
|`ghcr.io/stargz-containers/jruby:9.2.8.0-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/node:13.13.0-org`|Printing `hello`|
|`ghcr.io/stargz-containers/node:13.13.0-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/perl:5.30-org`|Printing `hello`|
|`ghcr.io/stargz-containers/perl:5.30-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/php:7.3.8-org`|Printing `hello`|
|`ghcr.io/stargz-containers/php:7.3.8-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/pypy:3.5-org`|Printing `hello`|
|`ghcr.io/stargz-containers/pypy:3.5-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/python:3.9-org`|Printing `hello`|
|`ghcr.io/stargz-containers/python:3.9-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/r-base:3.6.1-org`|Printing `hello`|
|`ghcr.io/stargz-containers/r-base:3.6.1-esgz`|Printing `hello`|
|`ghcr.io/stargz-containers/redis:5.0.5-org`|Code execution until up and ready message (`Ready to accept connections`) is printed|
|`ghcr.io/stargz-containers/redis:5.0.5-esgz`|Code execution until up and ready message (`Ready to accept connections`) is printed|
|`ghcr.io/stargz-containers/rethinkdb:2.3.6-org`|Code execution until up and ready message (`Server ready`) is printed|
|`ghcr.io/stargz-containers/rethinkdb:2.3.6-esgz`|Code execution until up and ready message (`Server ready`) is printed|
|`ghcr.io/stargz-containers/tomcat:10.0.0-jdk15-openjdk-buster-org`|Code execution until up and ready message (`Server startup`) is printed|
|`ghcr.io/stargz-containers/tomcat:10.0.0-jdk15-openjdk-buster-esgz`|Code execution until up and ready message (`Server startup`) is printed|
|`ghcr.io/stargz-containers/postgres:13.1-org`|Code execution until up and ready message (`database system is ready to accept connections`) is printed|
|`ghcr.io/stargz-containers/postgres:13.1-esgz`|Code execution until up and ready message (`database system is ready to accept connections`) is printed|
|`ghcr.io/stargz-containers/wordpress:5.7-org`|Code execution until up and ready message (`apache2 -D FOREGROUND`) is printed|
|`ghcr.io/stargz-containers/wordpress:5.7-esgz`|Code execution until up and ready message (`apache2 -D FOREGROUND`) is printed|
|`ghcr.io/stargz-containers/mariadb:10.5-org`|Code execution until up and ready message (`mysqld: ready for connections`) is printed|
|`ghcr.io/stargz-containers/mariadb:10.5-esgz`|Code execution until up and ready message (`mysqld: ready for connections`) is printed|
