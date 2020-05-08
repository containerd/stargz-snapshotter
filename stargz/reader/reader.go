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
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/google/crfs/stargz"
	"github.com/pkg/errors"
)

type Reader interface {
	OpenFile(name string) (io.ReaderAt, error)
	Lookup(name string) (*stargz.TOCEntry, bool)
	CacheTarGzWithReader(r io.Reader) error
}

func NewReader(sr *io.SectionReader, cache cache.BlobCache) (Reader, *stargz.TOCEntry, error) {
	r, err := stargz.Open(sr)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to parse stargz")
	}

	root, ok := r.Lookup("")
	if !ok {
		return nil, nil, fmt.Errorf("failed to get a TOCEntry of the root")
	}

	return &reader{
		r:     r,
		sr:    sr,
		cache: cache,
	}, root, nil
}

type reader struct {
	r     *stargz.Reader
	sr    *io.SectionReader
	cache cache.BlobCache
}

func (gr *reader) OpenFile(name string) (io.ReaderAt, error) {
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

func (gr *reader) Lookup(name string) (*stargz.TOCEntry, bool) {
	return gr.r.Lookup(name)
}

func (gr *reader) CacheTarGzWithReader(r io.Reader) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
		if h.Name == stargz.TOCTarName {
			// We don't need to cache prefetch landmarks and TOC json file.
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
					return err
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
		if cached, err := sf.cache.Fetch(id); err == nil && int64(len(cached)) == ce.ChunkSize {
			nr += copy(p[nr:], cached[offset+int64(nr)-ce.ChunkOffset:])
		} else {
			var (
				ip          []byte
				tmp         bool
				lowerUnread = positive(offset - ce.ChunkOffset)
				upperUnread = positive(ce.ChunkOffset + ce.ChunkSize - (offset + int64(len(p))))
			)
			if lowerUnread == 0 && upperUnread == 0 {
				ip = p[nr : int64(nr)+ce.ChunkSize]
			} else {
				// Use temporally buffer for aligning this chunk
				ip = make([]byte, ce.ChunkSize)
				tmp = true
			}
			n, err := sf.ra.ReadAt(ip, ce.ChunkOffset)
			if err != nil && err != io.EOF {
				return 0, errors.Wrap(err, "failed to read data")
			} else if int64(n) != ce.ChunkSize {
				return 0, fmt.Errorf("invalid chunk size %d; want %d", n, ce.ChunkSize)
			}
			if tmp {
				// Write temporally buffer to resulting slice
				n = copy(p[nr:], ip[lowerUnread:ce.ChunkSize-upperUnread])
				if int64(n) != ce.ChunkSize-upperUnread-lowerUnread {
					return 0, fmt.Errorf("unexpected final data size %d; want %d",
						n, ce.ChunkSize-upperUnread-lowerUnread)
				}
			}
			sf.cache.Add(id, ip)
			nr += n
		}
	}

	return nr, nil
}

func genID(digest string, offset, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", digest, offset, size)))
	return fmt.Sprintf("%x", sum)
}

func positive(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
