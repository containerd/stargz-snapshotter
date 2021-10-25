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

	"github.com/containerd/stargz-snapshotter/ipfs"
	httpapi "github.com/ipfs/go-ipfs-http-client"
	iface "github.com/ipfs/interface-go-ipfs-core"
	ipath "github.com/ipfs/interface-go-ipfs-core/path"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Reader struct {
	api  iface.CoreAPI
	path ipath.Path
}

func Supported(desc ocispec.Descriptor) bool {
	_, err := ipfs.GetPath(desc)
	return err == nil
}

func NewReader(ctx context.Context, desc ocispec.Descriptor) (*Reader, int64, error) {
	p, err := ipfs.GetPath(desc)
	if err != nil {
		return nil, 0, err
	}
	client, err := httpapi.NewLocalApi()
	if err != nil {
		return nil, 0, err
	}
	n, err := client.Unixfs().Get(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	if _, ok := n.(interface {
		io.ReaderAt
	}); !ok {
		return nil, 0, fmt.Errorf("ReaderAt is not implemented")
	}
	defer n.Close()
	s, err := n.Size()
	if err != nil {
		return nil, 0, err
	}
	return &Reader{client, p}, s, nil
}

func (r *Reader) Reader(ctx context.Context, off int64, size int64) (io.ReadCloser, error) {
	n, err := r.api.Unixfs().Get(ctx, r.path)
	if err != nil {
		return nil, err
	}
	ra, ok := n.(interface {
		io.ReaderAt
	})
	if !ok {
		return nil, fmt.Errorf("ReaderAt is not implemented")
	}
	return &reader{
		Reader:    io.NewSectionReader(ra, off, size),
		closeFunc: n.Close,
	}, nil
}

func (r *Reader) Check() error {
	n, err := r.api.Unixfs().Get(context.Background(), r.path)
	if err != nil {
		return err
	}
	if _, ok := n.(interface {
		io.ReaderAt
	}); !ok {
		return fmt.Errorf("ReaderAt is not implemented")
	}
	return n.Close()
}

func (r *Reader) GenID(off int64, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", r.path.String(), off, size)))
	return fmt.Sprintf("%x", sum)
}

type reader struct {
	io.Reader
	closeFunc func() error
}

func (r *reader) Close() error { return r.closeFunc() }
