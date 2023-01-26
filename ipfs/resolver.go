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

package ipfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"os"

	"github.com/containerd/containerd/remotes"
	ipfsclient "github.com/containerd/stargz-snapshotter/ipfs/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type resolver struct {
	scheme string
	client *ipfsclient.Client
}

type ResolverOptions struct {
	// Scheme is the scheme to fetch the specified IPFS content. "ipfs" or "ipns".
	Scheme string

	// IPFSPath is the path to the IPFS repository directory.
	IPFSPath string
}

func NewResolver(options ResolverOptions) (remotes.Resolver, error) {
	s := options.Scheme
	if s != "ipfs" && s != "ipns" {
		return nil, fmt.Errorf("unsupported scheme %q", s)
	}
	var ipath string
	if idir := os.Getenv("IPFS_PATH"); idir != "" {
		ipath = idir
	}
	if options.IPFSPath != "" {
		ipath = options.IPFSPath
	}
	// HTTP is only supported as of now. We can add https support here if needed (e.g. for connecting to it via proxy, etc)
	iurl, err := ipfsclient.GetIPFSAPIAddress(ipath, "http")
	if err != nil {
		return nil, fmt.Errorf("failed to get IPFS URL from ipfs path")
	}
	return &resolver{
		scheme: s,
		client: ipfsclient.New(iurl),
	}, nil
}

// Resolve resolves the provided ref for IPFS. ref must be a CID.
// TODO: Allow specifying IPFS path or URL. This requires to modify `reference` pkg because
//
//	it's incompatbile to the current reference specification.
func (r *resolver) Resolve(ctx context.Context, ref string) (name string, desc ocispec.Descriptor, err error) {
	rc, err := r.client.Get(path.Join("/", r.scheme, ref), nil, nil)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}
	defer rc.Close()
	if err := json.NewDecoder(rc).Decode(&desc); err != nil {
		return "", ocispec.Descriptor{}, err
	}
	if _, err := GetCID(desc); err != nil {
		return "", ocispec.Descriptor{}, err
	}
	return ref, desc, nil
}

func (r *resolver) Fetcher(ctx context.Context, ref string) (remotes.Fetcher, error) {
	return &fetcher{r}, nil
}

func (r *resolver) Pusher(ctx context.Context, ref string) (remotes.Pusher, error) {
	return nil, fmt.Errorf("immutable remote")
}

type fetcher struct {
	r *resolver
}

func (f *fetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	cid, err := GetCID(desc)
	if err != nil {
		return nil, err
	}
	return f.r.client.Get(path.Join("/", f.r.scheme, cid), nil, nil)
}
