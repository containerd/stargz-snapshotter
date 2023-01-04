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
	"os"
	"os/exec"
	"path"

	"github.com/containerd/containerd/remotes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type resolver struct {
	scheme   string
	ipfsPath string
}

type ResolverOptions struct {
	// Scheme is the scheme to fetch the specified IPFS content. "ipfs" or "ipns".
	Scheme string

	// IPFSPath is the path to the IPFS repository directory.
	IPFSPath string
}

// func NewResolver(client iface.CoreAPI, options ResolverOptions) (remotes.Resolver, error) {
func NewResolver(options ResolverOptions) (remotes.Resolver, error) {
	s := options.Scheme
	if s != "ipfs" && s != "ipns" {
		return nil, fmt.Errorf("unsupported scheme %q", s)
	}
	return &resolver{
		scheme:   s,
		ipfsPath: options.IPFSPath,
	}, nil
}

// Resolve resolves the provided ref for IPFS. ref must be a CID.
// TODO: Allow specifying IPFS path or URL. This requires to modify `reference` pkg because
//
//	it's incompatbile to the current reference specification.
func (r *resolver) Resolve(ctx context.Context, ref string) (name string, desc ocispec.Descriptor, err error) {
	rc, err := ipfsCat(path.Join("/", r.scheme, ref), r.ipfsPath)
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
	return ipfsCat(cid, f.r.ipfsPath)
}

func ipfsCat(p string, ipfsPath string) (io.ReadCloser, error) {
	cmd := exec.Command("ipfs", "cat", p)
	if ipfsPath != "" {
		cmd.Env = append(os.Environ(), fmt.Sprintf("IPFS_PATH=%s", ipfsPath))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		if _, err := io.Copy(pw, stdout); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := cmd.Wait(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return pr, nil
}
