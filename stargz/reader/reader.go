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
	"io/ioutil"
	"strings"
	"sync"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/google/crfs/stargz"
	"github.com/pkg/errors"
)

type Reader interface {
	OpenFile(name string) (io.ReaderAt, error)
	Lookup(name string) (*stargz.TOCEntry, bool)
	CacheTarGzWithReader(r io.Reader, opts ...cache.Option) error
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
		bufPool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	}, root, nil
}

type reader struct {
	r       *stargz.Reader
	sr      *io.SectionReader
	cache   cache.BlobCache
	bufPool sync.Pool
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
		gr:     gr,
	}, nil
}

func (gr *reader) Lookup(name string) (*stargz.TOCEntry, bool) {
	return gr.r.Lookup(name)
}

func (gr *reader) CacheTarGzWithReader(r io.Reader, opts ...cache.Option) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return errors.Wrapf(err, "failed to get gzip reader")
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				return errors.Wrapf(err, "failed to read next tar entry")
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

			// make sure that this range is at ce.ChunkOffset for ce.ChunkSize
			if nr != ce.ChunkOffset {
				return fmt.Errorf("invalid offset %d != %d", nr, ce.ChunkOffset)
			}

			// Check if the target chunks exists in the cache
			id := genID(fe.Digest, ce.ChunkOffset, ce.ChunkSize)
			if _, err := gr.cache.FetchAt(id, 0, nil, opts...); err != nil {
				// missed cache, needs to fetch and add it to the cache
				b := gr.bufPool.Get().(*bytes.Buffer)
				b.Reset()
				b.Grow(int(ce.ChunkSize))
				if _, err := io.CopyN(b, tr, ce.ChunkSize); err != nil {
					gr.bufPool.Put(b)
					return errors.Wrapf(err,
						"failed to read file payload of %q (offset:%d,size:%d)",
						h.Name, ce.ChunkOffset, ce.ChunkSize)
				}
				if int64(b.Len()) != ce.ChunkSize {
					gr.bufPool.Put(b)
					return fmt.Errorf("unexpected copied data size %d; want %d",
						b.Len(), ce.ChunkSize)
				}
				gr.cache.Add(id, b.Bytes()[:ce.ChunkSize], opts...)
				gr.bufPool.Put(b)

				nr += ce.ChunkSize
				continue
			}

			// Discard the target chunk
			if _, err := io.CopyN(ioutil.Discard, tr, ce.ChunkSize); err != nil {
				return errors.Wrapf(err,
					"failed to discard file payload of %q (offset:%d,size:%d)",
					h.Name, ce.ChunkOffset, ce.ChunkSize)
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
	gr     *reader
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
		var (
			id           = genID(sf.digest, ce.ChunkOffset, ce.ChunkSize)
			lowerDiscard = positive(offset - ce.ChunkOffset)
			upperDiscard = positive(ce.ChunkOffset + ce.ChunkSize - (offset + int64(len(p))))
			expectedSize = ce.ChunkSize - upperDiscard - lowerDiscard
		)

		// Check if the content exists in the cache
		n, err := sf.cache.FetchAt(id, lowerDiscard, p[nr:int64(nr)+expectedSize])
		if err == nil && int64(n) == expectedSize {
			nr += n
			continue
		}

		// We missed cache. Take it from underlying reader.
		// We read the whole chunk here and add it to the cache so that following
		// reads against neighboring chunks can take the data without decmpression.
		if lowerDiscard == 0 && upperDiscard == 0 {
			// We can directly store the result to the given buffer
			ip := p[nr : int64(nr)+ce.ChunkSize]
			n, err := sf.ra.ReadAt(ip, ce.ChunkOffset)
			if err != nil && err != io.EOF {
				return 0, errors.Wrap(err, "failed to read data")
			}
			sf.cache.Add(id, ip)
			nr += n
			continue
		}

		// Use temporally buffer for aligning this chunk
		b := sf.gr.bufPool.Get().(*bytes.Buffer)
		b.Reset()
		b.Grow(int(ce.ChunkSize))
		ip := b.Bytes()[:ce.ChunkSize]
		if _, err := sf.ra.ReadAt(ip, ce.ChunkOffset); err != nil && err != io.EOF {
			sf.gr.bufPool.Put(b)
			return 0, errors.Wrap(err, "failed to read data")
		}
		sf.cache.Add(id, ip)
		n = copy(p[nr:], ip[lowerDiscard:ce.ChunkSize-upperDiscard])
		sf.gr.bufPool.Put(b)
		if int64(n) != expectedSize {
			return 0, fmt.Errorf("unexpected final data size %d; want %d", n, expectedSize)
		}
		nr += n
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
