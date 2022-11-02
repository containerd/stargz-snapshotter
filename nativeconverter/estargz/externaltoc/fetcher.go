/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package externaltoc

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	esgzexternaltoc "github.com/containerd/stargz-snapshotter/estargz/externaltoc"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func NewRemoteDecompressor(ctx context.Context, hosts source.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) *esgzexternaltoc.GzipDecompressor {
	return esgzexternaltoc.NewGzipDecompressor(func() ([]byte, error) {
		resolver := docker.NewResolver(docker.ResolverOptions{
			Hosts: func(host string) ([]docker.RegistryHost, error) {
				if host != refspec.Hostname() {
					return nil, fmt.Errorf("unexpected host %q for image ref %q", host, refspec.String())
				}
				return hosts(refspec)
			},
		})
		return fetchTOCBlob(ctx, resolver, refspec, desc.Digest)
	})
}

func fetchTOCBlob(ctx context.Context, resolver remotes.Resolver, refspec reference.Spec, dgst digest.Digest) ([]byte, error) {
	// TODO: support custom location of TOC manifest and TOCs using annotations, etc.
	tocImgRef, err := getTOCReference(refspec.String())
	if err != nil {
		return nil, err
	}
	_, img, err := resolver.Resolve(ctx, tocImgRef)
	if err != nil {
		return nil, err
	}
	fetcher, err := resolver.Fetcher(ctx, tocImgRef)
	if err != nil {
		return nil, err
	}
	// TODO: cache this manifest
	manifest, err := containerdutil.FetchManifestPlatform(ctx, fetcher, img, platforms.DefaultSpec())
	if err != nil {
		return nil, err
	}
	return fetchTOCBlobFromManifest(ctx, fetcher, manifest, dgst)
}

func fetchTOCBlobFromManifest(ctx context.Context, fetcher remotes.Fetcher, manifest ocispec.Manifest, layerDigest digest.Digest) ([]byte, error) {
	for _, l := range manifest.Layers {
		if len(l.Annotations) == 0 {
			continue
		}
		ldgst, ok := l.Annotations["containerd.io/snapshot/stargz/layer.digest"]
		if !ok {
			continue
		}
		if ldgst == layerDigest.String() {
			r, err := fetcher.Fetch(ctx, l)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}
	}
	return nil, fmt.Errorf("TOC not found")
}
