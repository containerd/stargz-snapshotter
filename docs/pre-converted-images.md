# Trying pre-converted images

We have several pre-converted stargz images on Docker Hub, mainly for benchmarking purpose. This doc lists these images in a table format. You can try them on your machine with our snapshotter. Please refer to README for the procedure.

Please do not use them in production. You always can build your stargz images optimized for your workload, using `ctr-remote` command (Please also see README).

## Pre-converted images

This section contains a table of pre-converted images which can be used for benchmarking, testing, etc.

In the following table, `Image Name` column contains placeholders `{{ suffix }}` for image names. One of the following suffixes comes there based on the type of image.

- `org`: Legacy image copied from `docker.io/library` without optimization. Layers are normal tarballs.
- `sgz`: Stargz-formatted version of the `org` images. Layers are stargz archives and we can see performance improvement on the pull operation.
- `esgz`: Heavily optimized version of the `org` images, based on the workload. `ctr-remote images optimize` command is used for the optimization. We can see the performance improvement on run operation as well as pull.

`Optimized Workload` column describes workloads used for building `esgz` images. We optimized these images for benchmarking which is based on [HelloBench](https://github.com/Tintri/hello-bench) so we specified "hello-world"-like workloads for the command. See [benchmarking script](/script/benchmark/hello-bench/src/hello.py) for the exact command option specified for `ctr-remote images optimize`. 

|Image Name|Optimized Workload|
---|---
|`docker.io/stargz/alpine:3.10.2-{{ suffix }}`|Executing `echo hello` on the shell|
|`docker.io/stargz/drupal:8.7.6-{{ suffix }}`|Code execution until up and ready message (`apache2 -D FOREGROUND`) is printed|
|`docker.io/stargz/fedora:30-{{ suffix }}`|Executing `echo hello` on the shell|
|`docker.io/stargz/gcc:9.2.0-{{ suffix }}`|Compiling and executing a program which prints `hello`|
|`docker.io/stargz/glassfish:4.1-jdk8-{{ suffix }}`|Code execution until up and ready message (`Running GlassFish`) is printed|
|`docker.io/stargz/golang:1.12.9-{{ suffix }}`|Compiling and executing a program which prints `hello`|
|`docker.io/stargz/jenkins:2.60.3-{{ suffix }}`|Code execution until up and ready message (`Jenkins is fully up and running`) is printed|
|`docker.io/stargz/jruby:9.2.8.0-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/node:13.13.0-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/perl:5.30-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/php:7.3.8-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/pypy:3.5-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/python:3.7-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/r-base:3.6.1-{{ suffix }}`|Printing `hello`|
|`docker.io/stargz/redis:5.0.5-{{ suffix }}`|Code execution until up and ready message (`Ready to accept connections`) is printed|
|`docker.io/stargz/rethinkdb:2.3.6-{{ suffix }}`|Code execution until up and ready message (`Server ready`) is printed|
