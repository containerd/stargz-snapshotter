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
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/containerd/stargz-snapshotter/fs/remote"
	"github.com/containerd/stargz-snapshotter/ipfs"
	ipfsclient "github.com/containerd/stargz-snapshotter/ipfs/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ResolveHandler struct{}

func (r *ResolveHandler) Handle(ctx context.Context, desc ocispec.Descriptor) (remote.Fetcher, int64, error) {
	cid, err := ipfs.GetCID(desc)
	if err != nil {
		return nil, 0, err
	}
	var ipath string
	if idir := os.Getenv("IPFS_PATH"); idir != "" {
		ipath = idir
	}
	// HTTP is only supported as of now. We can add https support here if needed (e.g. for connecting to it via proxy, etc)
	iurl, err := ipfsclient.GetIPFSAPIAddress(ipath, "http")
	if err != nil {
		return nil, 0, err
	}
	client := ipfsclient.New(iurl)
	info, err := client.StatCID(cid)
	if err != nil {
		return nil, 0, err
	}
	return &fetcher{cid: cid, size: int64(info.Size), client: client}, int64(info.Size), nil
}

type fetcher struct {
	cid  string
	size int64

	client *ipfsclient.Client
}

func (f *fetcher) Fetch(ctx context.Context, off int64, size int64) (io.ReadCloser, error) {
	if off > f.size {
		return nil, fmt.Errorf("offset is larger than the size of the blob %d(offset) > %d(blob size)", off, f.size)
	}
	o, s := int(off), int(size)
	return f.client.Get("/ipfs/"+f.cid, &o, &s)
}

func (f *fetcher) Check() error {
	_, err := f.client.StatCID(f.cid)
	return err
}

func (f *fetcher) GenID(off int64, size int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s-%d-%d", f.cid, off, size))
	return fmt.Sprintf("%x", sum)
}
