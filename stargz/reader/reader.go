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

package reader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/crfs/stargz"
	"github.com/ktock/stargz-snapshotter/cache"
)

const (
	PrefetchLandmark = ".prefetch.landmark"
)

func NewReader(sr *io.SectionReader, cache cache.BlobCache) (*Reader, *stargz.TOCEntry, error) {
	r, err := stargz.Open(sr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse stargz: %v", err)
	}

	root, ok := r.Lookup("")
	if !ok {
		return nil, nil, fmt.Errorf("stargz: failed to get a TOCEntry of the root")
	}

	return &Reader{
		r:                      r,
		sr:                     sr,
		cache:                  cache,
		prefetchCompletionCond: sync.NewCond(&sync.Mutex{}),
	}, root, nil
}

type Reader struct {
	r                      *stargz.Reader
	sr                     *io.SectionReader
	cache                  cache.BlobCache
	prefetchInProgress     bool
	prefetchCompletionCond *sync.Cond
}

func (gr *Reader) OpenFile(name string) (io.ReaderAt, error) {
	sr, err := gr.r.OpenFile(name)
	if err != nil {
		return nil, err
	}
	e, ok := gr.r.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("failed to get TOCEntry %q", name)
	}
	return &file{
		name:   name,
		digest: e.Digest,
		r:      gr.r,
		cache:  gr.cache,
		ra:     sr,
	}, nil
}

func (gr *Reader) PrefetchWithReader(sr *io.SectionReader) error {
	gr.prefetchInProgress = true
	defer func() {
		gr.prefetchInProgress = false
		gr.prefetchCompletionCond.Broadcast()
	}()

	var prefetchSize int64
	if e, ok := gr.r.Lookup(PrefetchLandmark); ok {
		if e.Offset > sr.Size() {
			return fmt.Errorf("invalid landmark offset %d is larger than layer size %d",
				e.Offset, sr.Size())
		}
		prefetchSize = e.Offset
	}

	// Fetch specified range at once
	// TODO: when prefetchSize is too large, save memory by chunking the range
	prefetchBytes := make([]byte, prefetchSize)
	if _, err := io.ReadFull(sr, prefetchBytes); err != nil && err != io.EOF {
		return fmt.Errorf("failed to prefetch layer data: %v", err)
	}

	// Cache specified range to filesystem cache
	err := gr.CacheTarGzWithReader(bytes.NewReader(prefetchBytes))
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("error occurred during caching: %v", err)
	}
	return nil
}

func (gr *Reader) WaitForPrefetchCompletion(timeout time.Duration) error {
	waitUntilPrefetching := func() <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			if gr.prefetchInProgress {
				gr.prefetchCompletionCond.L.Lock()
				gr.prefetchCompletionCond.Wait()
				gr.prefetchCompletionCond.L.Unlock()
			}
			ch <- struct{}{}
		}()
		return ch
	}
	select {
	case <-time.After(timeout):
		gr.prefetchInProgress = false
		gr.prefetchCompletionCond.Broadcast()
		return fmt.Errorf("timeout(%v)", timeout)
	case <-waitUntilPrefetching():
		return nil
	}
}

func (gr *Reader) CacheTarGzWithReader(r io.Reader) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gzr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
		if h.Name == PrefetchLandmark || h.Name == stargz.TOCTarName {
			// We don't need to cache PrefetchLandmark and TOC json file.
			continue
		}
		fe, ok := gr.r.Lookup(strings.TrimSuffix(h.Name, "/"))
		if !ok {
			return fmt.Errorf("failed to get TOCEntry of %q", h.Name)
		}
		var nr int64
		for nr < h.Size {
			ce, ok := gr.r.ChunkEntryForOffset(h.Name, nr)
			if !ok {
				break
			}
			id := genID(fe.Digest, ce.ChunkOffset, ce.ChunkSize)
			if cacheData, err := gr.cache.Fetch(id); err != nil || len(cacheData) != int(ce.ChunkSize) {

				// make sure that this range is at ce.ChunkOffset for ce.ChunkSize
				if nr != ce.ChunkOffset {
					return fmt.Errorf("invalid offset %d != %d", nr, ce.ChunkOffset)
				}
				data := make([]byte, int(ce.ChunkSize))

				// Cache this chunk (offset: ce.ChunkOffset, size: ce.ChunkSize)
				if _, err := io.ReadFull(tr, data); err != nil && err != io.EOF {
					return fmt.Errorf("failed to read data: %v", err)
				}
				gr.cache.Add(id, data)
			}
			nr += ce.ChunkSize
		}
	}
	return nil
}

type file struct {
	name   string
	digest string
	ra     io.ReaderAt
	r      *stargz.Reader
	cache  cache.BlobCache
}

// ReadAt reads chunks from the stargz file with trying to fetch as many chunks
// as possible from the cache.
func (sf *file) ReadAt(p []byte, offset int64) (int, error) {
	nr := 0
	for nr < len(p) {
		ce, ok := sf.r.ChunkEntryForOffset(sf.name, offset+int64(nr))
		if !ok {
			break
		}
		id := genID(sf.digest, ce.ChunkOffset, ce.ChunkSize)
		data, err := sf.cache.Fetch(id)
		if err != nil || len(data) != int(ce.ChunkSize) {
			data = make([]byte, int(ce.ChunkSize))
			if _, err := sf.ra.ReadAt(data, ce.ChunkOffset); err != nil {
				if err != io.EOF {
					return 0, fmt.Errorf("failed to read data: %v", err)
				}
			}
			sf.cache.Add(id, data)
		}
		n := copy(p[nr:], data[offset+int64(nr)-ce.ChunkOffset:])
		nr += n
	}
	p = p[:nr]

	return len(p), nil
}

func genID(digest string, offset, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", digest, offset, size)))
	return fmt.Sprintf("%x", sum)
}
