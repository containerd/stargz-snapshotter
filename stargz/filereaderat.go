/*
   Copyright The containerd Authors.
   Copyright 2019 The Go Authors.

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

package stargz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"

	"github.com/google/crfs/stargz"
	"github.com/ktock/stargz-snapshotter/cache"
)

type fileReaderAt struct {
	name   string
	digest string
	gr     *stargzReader
	ra     io.ReaderAt
}

// ReadAt reads chunks from the stargz file with trying to fetch as many chunks
// as possible from the cache.
func (fr *fileReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	nr := 0
	for nr < len(p) {
		ce, ok := fr.gr.r.ChunkEntryForOffset(fr.name, offset+int64(nr))
		if !ok {
			break
		}
		id := fr.gr.genID(fr.digest, ce.ChunkOffset, ce.ChunkSize)
		data, err := fr.gr.cache.Fetch(id)
		if err != nil || len(data) != int(ce.ChunkSize) {
			data = make([]byte, int(ce.ChunkSize))
			if _, err := fr.ra.ReadAt(data, ce.ChunkOffset); err != nil {
				if err != io.EOF {
					return 0, fmt.Errorf("failed to read data: %v", err)
				}
			}
			fr.gr.cache.Add(id, data)
		}
		n := copy(p[nr:], data[offset+int64(nr)-ce.ChunkOffset:])
		nr += n
	}
	p = p[:nr]

	return len(p), nil
}

type stargzReader struct {
	r     *stargz.Reader
	cache cache.BlobCache
}

func (gr *stargzReader) openFile(name string) (io.ReaderAt, error) {
	sr, err := gr.r.OpenFile(name)
	if err != nil {
		return nil, err
	}
	e, ok := gr.r.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("failed to get TOCEntry %q", name)
	}
	return &fileReaderAt{
		name:   name,
		digest: e.Digest,
		gr:     gr,
		ra:     sr,
	}, nil
}

func (gr *stargzReader) prefetch(layer *io.SectionReader) (cache func() error, err error) {
	var prefetchSize int64
	if e, ok := gr.r.Lookup(PrefetchLandmark); ok {
		if e.Offset > layer.Size() {
			return nil, fmt.Errorf("invalid landmark offset %d is larger than layer size %d", e.Offset, layer.Size())
		}
		prefetchSize = e.Offset
	}

	// Fetch specified range at once (synchronously)
	// TODO: when prefetchSize is too large, save memory by chunking the range
	prefetchBytes := make([]byte, prefetchSize)
	if _, err := io.ReadFull(layer, prefetchBytes); err != nil && err != io.EOF {
		return nil, fmt.Errorf("failed to prefetch layer data: %v", err)
	}

	// Cache specified range to filesystem cache (asynchronously)
	cache = func() error {
		if err := gr.cacheTarGz(bytes.NewReader(prefetchBytes)); err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		return nil
	}

	return
}

func (gr *stargzReader) cacheTarGz(r io.Reader) error {
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
			id := gr.genID(fe.Digest, ce.ChunkOffset, ce.ChunkSize)
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

func (gr *stargzReader) genID(digest string, offset, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", digest, offset, size)))
	return fmt.Sprintf("%x", sum)
}
