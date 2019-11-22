// +build linux

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

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/google/crfs/stargz"
	"github.com/ktock/remote-snapshotter/cache"
)

type fileReaderAt struct {
	name string
	gr   *stargzReader
	ra   io.ReaderAt
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
		id := fr.gr.genID(ce.Name, ce.ChunkOffset, ce.ChunkSize)
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
	digest string
	r      *stargz.Reader
	cache  cache.BlobCache
}

func (gr *stargzReader) openFile(name string) (io.ReaderAt, error) {
	sr, err := gr.r.OpenFile(name)
	if err != nil {
		return nil, err
	}
	return &fileReaderAt{
		name: name,
		gr:   gr,
		ra:   sr,
	}, nil
}

func (gr *stargzReader) prefetch(layer *io.SectionReader, size int64) (<-chan struct{}, error) {
	done := make(chan struct{})
	if e, ok := gr.r.Lookup(prefetchLandmark); ok {
		size = e.Offset
	}
	if size == 0 {
		close(done)
		return done, nil
	}

	// Prefetch specified range at once
	raw := make([]byte, size)
	_, err := layer.ReadAt(raw, 0)
	if err != nil {
		if err != io.EOF {
			close(done)
			return done, fmt.Errorf("failed to get raw data: %v", err)
		}
	}

	go func() {
		defer close(done)

		// Parse the layer and cache chunks
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			payload, err := ioutil.ReadAll(tr)
			if err != nil {
				break
			}
			var nr int64
			for nr < h.Size {
				ce, ok := gr.r.ChunkEntryForOffset(h.Name, nr)
				if !ok {
					break
				}
				gr.cache.Add(gr.genID(ce.Name, ce.ChunkOffset, ce.ChunkSize),
					payload[ce.ChunkOffset:ce.ChunkOffset+ce.ChunkSize])
				nr += ce.ChunkSize
			}
		}
		return
	}()

	return done, nil
}

func (gr *stargzReader) genID(name string, offset, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%d-%d",
		gr.digest, name, offset, size)))
	return fmt.Sprintf("%x", sum)
}
