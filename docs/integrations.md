# Integrations

This document will help you experience how to integrate stargz snapshotter with the other libraries.

## Dragonfly

If you use stargz snapshotter to pull images in a large kubernetes cluster,
downloading each image layer will generate more range requests,
and the origin will handle more requests.

In this case it is recommended to use [dragonfly](https://d7y.io)
as registry mirror to reduce the requests and traffic of the origin based on P2P.

### Configure dragonfly as registry mirror

Change the `/etc/containerd-stargz-grpc/config.toml` configuration to make dragonfly as registry mirror.
`127.0.0.1:65001` is the proxy address of dragonfly peer,
and the `X-Dragonfly-Registry` header is the address of origin registry,
which is provided for dragonfly to download the images.

```toml
[[resolver.host."docker.io".mirrors]]
  host = "127.0.0.1:65001"
  insecure = true
  [resolver.host."docker.io".mirrors.header]
    X-Dragonfly-Registry = ["https://index.docker.io"]
```

For more details about dragonfly as registry mirror,
refer to [How To Use Dragonfly With eStargz](https://d7y.io/docs/setup/integration/stargz/).
