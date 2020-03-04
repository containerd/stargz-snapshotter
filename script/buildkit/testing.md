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

```bash
# "${BUILDKIT_STARGZ}/buildctl" build --progress=plain \
       --frontend=dockerfile.v0 \
       --local context=./script/buildkit/sample_sgz \
       --local dockerfile=./script/buildkit/sample_sgz \
       --output type=docker,name=sample,dest=/buildkit-sample.tar
```

You can check the contents from outside the testing container.

```
$ docker cp buildkit-integration:/buildkit-sample.tar /tmp/
$ BLOBPATH=$(cat /tmp/buildkit-sample.tar | tar -xf - 'manifest.json' -O | jq -r '.[0].Layers[1]')
$ cat /tmp/buildkit-sample.tar | tar -xf - "${BLOBPATH}" -O | tar -zxf - 'hello.txt' -O
hello
$ rm /tmp/buildkit-sample.tar
```

## Cleaning up(on the host)

```
$ docker-compose -f "${DOCKER_COMPOSE_TMP}" down -v
$ rm "${DOCKER_COMPOSE_TMP}"
```
