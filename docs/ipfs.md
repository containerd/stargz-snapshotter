# Running containers on IPFS (experimental)

:information_source: This document isn't for Kubernetes environemnt. For information about node-to-node image sharing on Kubernetes, please refer to [the docs in nerdctl project](https://github.com/containerd/nerdctl/tree/main/examples/nerdctl-ipfs-registry-kubernetes).

You can run OCI-compatible container images on IPFS with lazy pulling.

To enable this feature, add the following configuration to `config.toml` of Stargz Snapsohtter (typically located at `/etc/containerd-stargz-grpc/config.toml`).

```toml
ipfs = true
```

> NOTE: containerd-stargz-grpc tries to connect to IPFS API written in `~/.ipfs/api` (or the file under `$IPFS_PATH` if configured) via HTTP (not HTTPS).

## IPFS-enabled OCI Image

For obtaining IPFS-enabled OCI Image, each descriptor in an OCI image must contain the following [IPFS URL](https://docs.ipfs.io/how-to/address-ipfs-on-web/#native-urls) in `urls` field.

```
ipfs://<CID>
```

`<CID>` is the Base32 case-insensitive CIDv1 of the blob that the descriptor points to.

An image is represented as a CID pointing to the OCI descriptor of the top-level blob of the image (i.e. image index).

The following is an example OCI descriptor pointing to the image index of an IPFS-enabled image:

```console
# ipfs cat bafkreie7754qk7fl56ebauawdgfuqqa3kdd7sotvuhsm6wbz3qin6ssw3a | jq
{
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "digest": "sha256:80d6aec48c0a74635a5f3dc106328c1673afaa21ed6e1270a9a44de66e8ffa55",
  "size": 314,
  "urls": [
    "ipfs://bafkreiea22xmjdakorrvuxz5yeddfdawoox2uipnnyjhbknejxtg5d72ku"
  ]
}
```

## Lazy pulling with Stargz Snapshotter

If layer descriptors of an image contain the URLs described above and these blobs are formatted as eStargz, Stargz Snapshotter mounts them from IPFS to the container's rootfs using FUSE with lazy pulling support.
Thus container can startup without waiting for the entire image contents being locally available.
Necessary chunks of contents (e.g. each file in the rootfs) are fetched from IPFS on-demand.

If the container image isn't eStargz or the snapshotter isn't Stargz Snapshotter (e.g. overlayfs snapshotter), containerd fetches the entire image contents from IPFS and unpacks it to the local directory before starting the container.
Thus possibly you'll see slow container cold-start.

## Examples

This section describes some examples of storing images to IPFS and running them as containers.
Make sure IPFS daemon runs on your node.
For example, you can run an IPFS daemon using the following command.

```
ipfs daemon
```

:information_source: If you don't want IPFS to communicate with nodes on the internet, you can run IPFS daemon in offline mode using `--offline` flag or you can create a private IPFS network as described in Appendix 1.

### Running a container with lazy pulling

`ctr-remote image ipfs-push` command converts an image to IPFS-enabled eStargz and stores it to IPFS.

```console
# ctr-remote i pull ghcr.io/stargz-containers/python:3.9-org
# ctr-remote i ipfs-push ghcr.io/stargz-containers/python:3.9-org
bafkreie7754qk7fl56ebauawdgfuqqa3kdd7sotvuhsm6wbz3qin6ssw3a
```

The printed IPFS CID (`bafkreie7754qk7fl56ebauawdgfuqqa3kdd7sotvuhsm6wbz3qin6ssw3a`) points to an OCI descriptor which points to the image index of the added image.

```console
# ipfs cat bafkreie7754qk7fl56ebauawdgfuqqa3kdd7sotvuhsm6wbz3qin6ssw3a | jq
{
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "digest": "sha256:80d6aec48c0a74635a5f3dc106328c1673afaa21ed6e1270a9a44de66e8ffa55",
  "size": 314,
  "urls": [
    "ipfs://bafkreiea22xmjdakorrvuxz5yeddfdawoox2uipnnyjhbknejxtg5d72ku"
  ]
}
```

You can run this image from IPFS using that CID as an image reference for `ctr-remote image rpull`.
`--ipfs` option is needed for enabling this.

Note that `ctr-remote` accepts an IPFS CID as the image reference but doesn't support `/ipfs`-prefixed path as of now.
We're working on eliminating this limitation.

```console
# time ( ctr-remote i rpull --ipfs bafkreie7754qk7fl56ebauawdgfuqqa3kdd7sotvuhsm6wbz3qin6ssw3a && \
         ctr-remote run --snapshotter=stargz --rm -t bafkreie7754qk7fl56ebauawdgfuqqa3kdd7sotvuhsm6wbz3qin6ssw3a foo python -c 'print("Hello, World!")' )
fetching sha256:80d6aec4... application/vnd.oci.image.index.v1+json
fetching sha256:16d36f86... application/vnd.oci.image.manifest.v1+json
fetching sha256:236b4bd7... application/vnd.oci.image.config.v1+json
Hello, World!

real	0m1.099s
user	0m0.047s
sys	0m0.037s
```

### Running a container without lazy pulling

Though eStargz-based lazy pulling is highly recommended for speeding up the container startup time, you can store and run non-eStargz images with IPFS as well.
In this case, containerd fetches the entire image contents from IPFS and unpacks it to the local directory before starting the container.

You can add a non-eStargz image to IPFS using `--estargz=false` option.

```console
# ctr-remote i pull ghcr.io/stargz-containers/python:3.9-org
# ctr-remote i ipfs-push --estargz=false ghcr.io/stargz-containers/python:3.9-org
bafkreienbir4knaofs3o5f57kqw2the2v7zdhdlzpkq346mipuopwvqhty
```

You don't need FUSE nor stargz snapshotter for running this image but will see slow container cold-start.
This example uses overlayfs snapshotter of containerd.

```console
# time ( ctr-remote i rpull --snapshotter=overlayfs --ipfs bafkreienbir4knaofs3o5f57kqw2the2v7zdhdlzpkq346mipuopwvqhty && \
         ctr-remote run --snapshotter=overlayfs --rm -t bafkreienbir4knaofs3o5f57kqw2the2v7zdhdlzpkq346mipuopwvqhty foo python -c 'print("Hello, World!")' )
fetching sha256:7240ac9f... application/vnd.oci.image.index.v1+json
fetching sha256:17dc54f4... application/vnd.oci.image.manifest.v1+json
fetching sha256:6f1289b1... application/vnd.oci.image.config.v1+json
fetching sha256:9476e460... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:64c0f10e... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:4c25b309... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:942374d5... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:3fff52a3... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:5cf06daf... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:419e258e... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:1acf5650... application/vnd.oci.image.layer.v1.tar+gzip
fetching sha256:b95c0dd0... application/vnd.oci.image.layer.v1.tar+gzip
Hello, World!

real	0m11.320s
user	0m0.556s
sys	0m0.280s
```

## Appendix 1: Creating IPFS private network

You can create a private IPFS network as described in the official docs.

- https://github.com/ipfs/go-ipfs/blob/v0.10.0/docs/experimental-features.md#private-networks

The following is the summary.

First, generate a key and save it to `~/.ipfs/swarm.key` (or under `$IPFS_PATH` if configured) of nodes you want to have in the network.
IPFS only connects to peers having this key.

```
go install github.com/Kubuxu/go-ipfs-swarm-key-gen/ipfs-swarm-key-gen@latest
~/go/bin/ipfs-swarm-key-gen > ~/.ipfs/swarm.key
```

Select nodes as a bootstrap nodes.
IPFS daemons learn about the peers on the private network from them.
Configure all non-bootstrap nodes to recognize only our bootstrap nodes instead of public ones like the following example.

```
ipfs bootstrap rm --all
ipfs bootstrap add /ip4/<ip address of bootstrap node>/tcp/4001/ipfs/<Peer ID of the bootstrap node>
```

:information_source: You can get Peer ID of a node by `ipfs config show | grep "PeerID"`.

Finally, start all nodes in the private network.

```
export LIBP2P_FORCE_PNET=1
ipfs daemon
```

`LIBP2P_FORCE_PNET=1` makes sure that the daemon uses the private network and fails if the private network isn't configured.
