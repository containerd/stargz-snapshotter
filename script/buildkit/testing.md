# Testing the integration with buildkit(WIP)

Stargz snapshotter's integration with buildkit is WIP but we can test the limited functionalities with our patched version of buildkit.

## Preparing the testing environment

```bash
$ cd ./script/buildkit
$ DOCKER_COMPOSE_TMP=$(mktemp)
$ ./docker-compose-buildkit.yml.sh "$(realpath ../../)" "buildkit-integration" > "${DOCKER_COMPOSE_TMP}"
$ docker-compose -f "${DOCKER_COMPOSE_TMP}" build \
                 --build-arg HTTP_PROXY=$HTTP_PROXY \
                 --build-arg HTTPS_PROXY=$HTTP_PROXY \
                 --build-arg http_proxy=$HTTP_PROXY \
                 --build-arg https_proxy=$HTTP_PROXY \
                 buildkit_integration
$ docker-compose -f "${DOCKER_COMPOSE_TMP}" up -d
$ docker exec -it buildkit-integration /bin/bash
```

## Running buildkitd(inside the testing container)

### With OCI worker

```bash
# BUILDKIT_BIN_PATH="${BUILDKIT_STARGZ}" ./script/buildkit/run.sh \
      --oci-worker-snapshotter=stargz \
      --oci-worker-proxy-snapshotter-address=/run/containerd-stargz-grpc/containerd-stargz-grpc.sock
```

### With containerd worker

```bash
# BUILDKIT_BIN_PATH="${BUILDKIT_STARGZ}" ./script/buildkit/run.sh \
       --oci-worker=false --containerd-worker=true \
       --containerd-worker-snapshotter=stargz
```

## Building a sample image

### Without exporting
```bash
# "${BUILDKIT_STARGZ}/buildctl" build --progress=plain \
       --frontend=dockerfile.v0 \
       --local context=./script/buildkit/sample_sgz \
       --local dockerfile=./script/buildkit/sample_sgz
```

### With exporting

Currently, we have two problems for exporting.

1. Archiving layers takes a long time. It is because of the low READ performance including fetching contents from the registry.
2. We only support exporting type `tar`. It is because when we use remote snapshotters, containerd doesn't store the image contents in the content store but buildkit needs to use these contents for exporting images.

We can export `tar` with the following command.

```bash
# "${BUILDKIT_STARGZ}/buildctl" build --progress=plain \
       --frontend=dockerfile.v0 \
       --local context=./script/buildkit/sample_sgz \
       --local dockerfile=./script/buildkit/sample_sgz \
       --output type=tar,dest=/tmp/buildkit-sample.tar
# cat /tmp/buildkit-sample.tar | tar -xf - 'hello.txt' -O
hello
```

## Cleaning up(on the host)

```
$ docker-compose -f "${DOCKER_COMPOSE_TMP}" down -v
$ rm "${DOCKER_COMPOSE_TMP}"
```
