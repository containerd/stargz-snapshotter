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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/pkg/errors"
)

var contentRangeRegexp = regexp.MustCompile(`bytes ([0-9]+)-([0-9]+)/([0-9]+|\\*)`)

type Blob interface {
	Authn(tr http.RoundTripper) (http.RoundTripper, error)
	Check() error
	Size() int64
	FetchedSize() int64
	ReadAt(p []byte, offset int64, opts ...Option) (int, error)
	Cache(offset int64, size int64, opts ...Option) error
}

type blob struct {
	fetcher   *fetcher
	fetcherMu sync.Mutex

	size          int64
	keychain      authn.Keychain
	chunkSize     int64
	cache         cache.BlobCache
	lastCheck     time.Time
	checkInterval time.Duration

	fetchedRegionSet   regionSet
	fetchedRegionSetMu sync.Mutex
}

func (b *blob) Authn(tr http.RoundTripper) (http.RoundTripper, error) {
	b.fetcherMu.Lock()
	fr := b.fetcher
	b.fetcherMu.Unlock()
	return fr.authn(tr, b.keychain)
}

func (b *blob) Check() error {
	now := time.Now()
	if now.Sub(b.lastCheck) < b.checkInterval {
		// do nothing if not expired
		return nil
	}
	b.lastCheck = now
	b.fetcherMu.Lock()
	fr := b.fetcher
	b.fetcherMu.Unlock()
	return fr.check()
}

func (b *blob) Size() int64 {
	return b.size
}

func (b *blob) FetchedSize() int64 {
	b.fetchedRegionSetMu.Lock()
	sz := b.fetchedRegionSet.totalSize()
	b.fetchedRegionSetMu.Unlock()
	return sz
}

func (b *blob) Cache(offset int64, size int64, opts ...Option) error {
	fetchReg := region{floor(offset, b.chunkSize), ceil(offset+size-1, b.chunkSize) - 1}
	discard := make(map[region]io.Writer)
	b.walkChunks(fetchReg, func(reg region) error {
		discard[reg] = ioutil.Discard // do not read chunks (only cached)
		return nil
	})
	if err := b.fetchRange(discard, opts...); err != nil {
		return err
	}

	return nil
}

// ReadAt reads remote chunks from specified offset for the buffer size.
// It tries to fetch as many chunks as possible from local cache.
// We can configure this function with options.
func (b *blob) ReadAt(p []byte, offset int64, opts ...Option) (int, error) {
	if len(p) == 0 || offset > b.size {
		return 0, nil
	}

	// Make the buffer chunk aligned
	allRegion := region{floor(offset, b.chunkSize), ceil(offset+int64(len(p))-1, b.chunkSize) - 1}
	allData := make(map[region]io.Writer)
	var commits []func() error
	b.walkChunks(allRegion, func(chunk region) error {
		var (
			ip          []byte
			base        = positive(chunk.b - offset)
			lowerUnread = positive(offset - chunk.b)
			upperUnread = positive(chunk.e + 1 - (offset + int64(len(p))))
		)
		if lowerUnread == 0 && upperUnread == 0 {
			ip = p[base : base+chunk.size()]
		} else {
			// Use temporally buffer for aligning this chunk
			ip = make([]byte, chunk.size())
			commits = append(commits, func() error {
				n := copy(p[base:], ip[lowerUnread:chunk.size()-upperUnread])
				if int64(n) != chunk.size()-upperUnread-lowerUnread {
					return fmt.Errorf("unexpected data size %d; want %d",
						n, chunk.size()-upperUnread-lowerUnread)
				}
				return nil
			})
		}
		allData[chunk] = &byteWriter{
			p: ip,
		}
		return nil
	})

	// Read required data
	if err := b.fetchRange(allData, opts...); err != nil {
		return 0, err
	}

	// Write all data to the result buffer
	for _, c := range commits {
		if err := c(); err != nil {
			return 0, err
		}
	}

	// Adjust the buffer size according to the blob size
	if remain := b.size - offset; int64(len(p)) >= remain {
		if remain < 0 {
			remain = 0
		}
		p = p[:remain]
	}

	return len(p), nil
}

// fetchRange fetches all specified chunks from local cache and remote blob.
func (b *blob) fetchRange(allData map[region]io.Writer, opts ...Option) error {
	// Fetcher can be suddenly updated so we take and use the snapshot of it for
	// consistency.
	b.fetcherMu.Lock()
	fr := b.fetcher
	b.fetcherMu.Unlock()

	// Read data from cache
	fetched := make(map[region]bool)
	for chunk := range allData {
		data, err := b.cache.Fetch(fr.genID(chunk))
		if err != nil || int64(len(data)) != chunk.size() {
			fetched[chunk] = false // missed cache, needs to fetch remotely.
			continue
		}
		if n, err := io.Copy(allData[chunk], bytes.NewReader(data)); err != nil {
			return err
		} else if n != chunk.size() {
			return fmt.Errorf("unexpected cached data size %d; want %d", n, chunk.size())
		}
	}
	if len(fetched) == 0 {
		// We successfully served whole range from cache
		return nil
	}

	// request missed regions
	var req []region
	for reg := range fetched {
		req = append(req, reg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mr, err := fr.fetch(ctx, req, opts...)
	if err != nil {
		return err
	}
	defer mr.Close()

	// Update the check timer because we succeeded to access the blob
	b.lastCheck = time.Now()

	// chunk and cache responsed data. Regions must be aligned by chunk size.
	// TODO: Reorganize remoteData to make it be aligned by chunk size
	for {
		reg, p, err := mr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return errors.Wrapf(err, "failed to read multipart resp")
		}
		if err := b.walkChunks(reg, func(chunk region) error {
			data := make([]byte, chunk.size())
			if _, err := io.ReadFull(p, data); err != nil {
				return err
			}
			b.cache.Add(fr.genID(chunk), data)
			b.fetchedRegionSetMu.Lock()
			b.fetchedRegionSet.add(chunk)
			b.fetchedRegionSetMu.Unlock()
			if _, ok := fetched[chunk]; ok {
				fetched[chunk] = true
				if n, err := io.Copy(allData[chunk], bytes.NewReader(data)); err != nil {
					return errors.Wrap(err, "failed to write chunk to buffer")
				} else if n != chunk.size() {
					return fmt.Errorf("unexpected fetched data size %d; want %d",
						n, chunk.size())
				}
			}
			return nil
		}); err != nil {
			return errors.Wrapf(err, "failed to get chunks")
		}
	}

	// Check all chunks are fetched
	var unfetched []region
	for c, b := range fetched {
		if !b {
			unfetched = append(unfetched, c)
		}
	}
	if unfetched != nil {
		return fmt.Errorf("failed to fetch region %v", unfetched)
	}

	return nil
}

type walkFunc func(reg region) error

// walkChunks walks chunks from begin to end in order in the specified region.
// specified region must be aligned by chunk size.
func (b *blob) walkChunks(allRegion region, walkFn walkFunc) error {
	if allRegion.b%b.chunkSize != 0 {
		return fmt.Errorf("region (%d, %d) must be aligned by chunk size",
			allRegion.b, allRegion.e)
	}
	for i := allRegion.b; i <= allRegion.e && i < b.size; i += b.chunkSize {
		reg := region{i, i + b.chunkSize - 1}
		if reg.e >= b.size {
			reg.e = b.size - 1
		}
		if err := walkFn(reg); err != nil {
			return err
		}
	}
	return nil
}

type byteWriter struct {
	p []byte
	n int
}

func (w *byteWriter) Write(p []byte) (int, error) {
	n := copy(w.p[w.n:], p)
	w.n += n
	return n, nil
}

func floor(n int64, unit int64) int64 {
	return (n / unit) * unit
}

func ceil(n int64, unit int64) int64 {
	return (n/unit + 1) * unit
}

func positive(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
