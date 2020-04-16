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
	"fmt"
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

// ReadAt reads remote chunks from specified offset for the buffer size.
// It tries to fetch as many chunks as possible from local cache.
// We can configure this function with options.
func (b *blob) ReadAt(p []byte, offset int64, opts ...Option) (int, error) {
	if len(p) == 0 || offset > b.size {
		return 0, nil
	}

	// Fetch all data.
	allRegion := region{floor(offset, b.chunkSize), ceil(offset+int64(len(p))-1, b.chunkSize) - 1}
	allData, err := b.fetchRange(allRegion, opts...)
	if err != nil {
		return 0, err
	}

	// Write all chunks to the result buffer
	regionData := make([]byte, 0, allRegion.size())
	if err := b.walkChunks(allRegion, func(reg region) error {
		data := allData[reg]
		if int64(len(data)) != reg.size() {
			return fmt.Errorf("fetched chunk(%d, %d) size is invalid", reg.b, reg.e)
		}
		regionData = append(regionData, data...)
		return nil
	}); err != nil {
		return 0, errors.Wrapf(err, "failed to gather chunks for region (%d, %d)",
			allRegion.b, allRegion.e)
	}
	if remain := b.size - offset; int64(len(p)) > remain {
		if remain < 0 {
			remain = 0
		}
		p = p[:remain]
	}
	base := offset - allRegion.b // relative offset from the base of the fetched region
	copy(p, regionData[base:base+int64(len(p))])
	return len(p), nil
}

// fetchRange fetches all specified chunks from local cache and remote blob.
// target region must be aligned by chunk size.
func (b *blob) fetchRange(target region, opts ...Option) (map[region][]byte, error) {
	var (
		// all data to be returned. it must be chunked to chunkSize
		allData = map[region][]byte{}

		// squashed requesting chunks for reducing the total size of request header
		// (servers generally have limits for the size of headers)
		// TODO: when our request has too many ranges, we need to divide it into
		//       multiple requests to avoid huge header.
		requests = regionSet{}
	)

	// Fetcher can be suddenly updated so we take and use the snapshot of it for
	// consistency.
	b.fetcherMu.Lock()
	fr := b.fetcher
	b.fetcherMu.Unlock()

	// Fetch data from cache
	if err := b.walkChunks(target, func(chunk region) error {
		data, err := b.cache.Fetch(fr.genID(chunk))
		if err != nil || int64(len(data)) != chunk.size() {
			requests.add(chunk) // missed cache, needs to fetch remotely.
			return nil
		}
		allData[chunk] = data
		return nil
	}); err != nil {
		return nil, err
	} else if requests.totalSize() == 0 {
		// We successfully served whole range from cache
		return allData, nil
	}

	// request missed regions
	remoteData, err := fr.fetch(requests.rs, opts...)
	if err != nil {
		return nil, err
	}

	// chunk and cache responsed data
	// TODO: Reorganize remoteData to make it be aligned by chunk size
	b.fetchedRegionSetMu.Lock()
	defer b.fetchedRegionSetMu.Unlock()
	for reg, data := range remoteData {
		if err := b.walkChunks(reg, func(chunk region) error {
			var (
				base = chunk.b - reg.b
				end  = base + chunk.size()
			)
			if int64(len(data)) < end {
				return fmt.Errorf("invalid remote data size %d; want at least %d",
					len(data), end)
			}
			allData[chunk] = data[base:end]
			b.cache.Add(fr.genID(chunk), data[base:end])
			b.fetchedRegionSet.add(chunk)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return allData, nil
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

func floor(n int64, unit int64) int64 {
	return (n / unit) * unit
}

func ceil(n int64, unit int64) int64 {
	return (n/unit + 1) * unit
}
