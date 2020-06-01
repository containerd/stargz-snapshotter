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
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/google/crfs/stargz"
)

const (
	sampleChunkSize    = 3
	sampleMiddleOffset = sampleChunkSize / 2
	sampleData1        = "0123456789"
	lastChunkOffset1   = sampleChunkSize * (int64(len(sampleData1)) / sampleChunkSize)
)

// Tests Reader for failure cases.
func TestFailReader(t *testing.T) {
	testFileName := "test"
	stargzFile := buildStargz(t, []tarent{
		regfile(testFileName, sampleData1),
	}, chunkSizeInfo(sampleChunkSize))
	br := &breakReaderAt{
		ReaderAt: stargzFile,
		success:  true,
	}
	bsr := io.NewSectionReader(br, 0, stargzFile.Size())
	gr, _, err := newReader(bsr, &nopCache{})
	if err != nil {
		t.Fatalf("Failed to open stargz file: %v", err)
	}

	// tests for opening file
	_, err = gr.OpenFile("dummy")
	if err == nil {
		t.Errorf("succeeded to open file but wanted to fail")
		return
	}

	fr, err := gr.OpenFile(testFileName)
	if err != nil {
		t.Errorf("failed to open file but wanted to succeed: %v", err)
	}
	p := make([]byte, len(sampleData1))

	// tests for reading file
	br.success = true
	n, err := fr.ReadAt(p, 0)
	if err != nil || n != len(sampleData1) || !bytes.Equal([]byte(sampleData1), p) {
		t.Errorf("failed to read data but wanted to succeed: %v", err)
		return
	}

	br.success = false
	_, err = fr.ReadAt(p, 0)
	if err == nil {
		t.Errorf("succeeded to read data but wanted to fail")
		return
	}
}

type breakReaderAt struct {
	io.ReaderAt
	success bool
}

func (br *breakReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if br.success {
		return br.ReaderAt.ReadAt(p, off)
	}
	return 0, fmt.Errorf("failed")
}

type nopCache struct{}

func (nc *nopCache) FetchAt(key string, offset int64, p []byte, opts ...cache.Option) (int, error) {
	return 0, fmt.Errorf("Missed cache: %q", key)
}

func (nc *nopCache) Add(key string, p []byte, opts ...cache.Option) {}

type testCache struct {
	membuf map[string]string
	t      *testing.T
	mu     sync.Mutex
}

func (tc *testCache) FetchAt(key string, offset int64, p []byte, opts ...cache.Option) (int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	cache, ok := tc.membuf[key]
	if !ok {
		return 0, fmt.Errorf("Missed cache: %q", key)
	}
	return copy(p, cache[offset:]), nil
}

func (tc *testCache) Add(key string, p []byte, opts ...cache.Option) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.membuf[key] = string(p)
	tc.t.Logf("  cached [%s...]: %q", key[:8], string(p))
}

type region struct{ b, e int64 }

// Tests ReadAt method of each file.
func TestFileReadAt(t *testing.T) {
	sizeCond := map[string]int64{
		"single_chunk": sampleChunkSize - sampleMiddleOffset,
		"multi_chunks": sampleChunkSize + sampleMiddleOffset,
	}
	innerOffsetCond := map[string]int64{
		"at_top":    0,
		"at_middle": sampleMiddleOffset,
	}
	baseOffsetCond := map[string]int64{
		"of_1st_chunk":  sampleChunkSize * 0,
		"of_2nd_chunk":  sampleChunkSize * 1,
		"of_last_chunk": lastChunkOffset1,
	}
	fileSizeCond := map[string]int64{
		"in_1_chunk_file":  sampleChunkSize * 1,
		"in_2_chunks_file": sampleChunkSize * 2,
		"in_max_size_file": int64(len(sampleData1)),
	}
	cacheCond := map[string][]region{
		"with_clean_cache": nil,
		"with_edge_filled_cache": {
			region{0, sampleChunkSize - 1},
			region{lastChunkOffset1, int64(len(sampleData1)) - 1},
		},
		"with_sparse_cache": {
			region{0, sampleChunkSize - 1},
			region{2 * sampleChunkSize, 3*sampleChunkSize - 1},
		},
	}
	for sn, size := range sizeCond {
		for in, innero := range innerOffsetCond {
			for bo, baseo := range baseOffsetCond {
				for fn, filesize := range fileSizeCond {
					for cc, cacheExcept := range cacheCond {
						t.Run(fmt.Sprintf("reading_%s_%s_%s_%s_%s", sn, in, bo, fn, cc), func(t *testing.T) {
							if filesize > int64(len(sampleData1)) {
								t.Fatal("sample file size is larger than sample data")
							}

							wantN := size
							offset := baseo + innero
							if remain := filesize - offset; remain < wantN {
								if wantN = remain; wantN < 0 {
									wantN = 0
								}
							}

							// use constant string value as a data source.
							want := strings.NewReader(sampleData1)

							// data we want to get.
							wantData := make([]byte, wantN)
							_, err := want.ReadAt(wantData, offset)
							if err != nil && err != io.EOF {
								t.Fatalf("want.ReadAt (offset=%d,size=%d): %v", offset, wantN, err)
							}

							// data we get through a file.
							f := makeFile(t, []byte(sampleData1)[:filesize], sampleChunkSize)
							f.ra = newExceptSectionReader(t, f.ra, cacheExcept...)
							for _, reg := range cacheExcept {
								f.cache.Add(genID(f.digest, reg.b, reg.e-reg.b+1), []byte(sampleData1[reg.b:reg.e+1]))
							}
							respData := make([]byte, size)
							n, err := f.ReadAt(respData, offset)
							if err != nil {
								t.Errorf("failed to read off=%d, size=%d, filesize=%d: %v", offset, size, filesize, err)
								return
							}
							respData = respData[:n]

							if !bytes.Equal(wantData, respData) {
								t.Errorf("off=%d, filesize=%d; read data{size=%d,data=%q}; want (size=%d,data=%q)",
									offset, filesize, len(respData), string(respData), wantN, string(wantData))
								return
							}

							// check cache has valid contents.
							cn := 0
							nr := 0
							for int64(nr) < wantN {
								ce, ok := f.r.ChunkEntryForOffset(f.name, offset+int64(nr))
								if !ok {
									break
								}
								data := make([]byte, ce.ChunkSize)
								n, err := f.cache.FetchAt(genID(f.digest, ce.ChunkOffset, ce.ChunkSize), 0, data)
								if err != nil || n != int(ce.ChunkSize) {
									t.Errorf("missed cache of offset=%d, size=%d: %v(got size=%d)", ce.ChunkOffset, ce.ChunkSize, err, n)
									return
								}
								nr += n
								cn++
							}
						})
					}
				}
			}
		}
	}
}

type exceptSectionReader struct {
	ra     io.ReaderAt
	except map[region]bool
	t      *testing.T
}

func newExceptSectionReader(t *testing.T, ra io.ReaderAt, except ...region) io.ReaderAt {
	er := exceptSectionReader{ra: ra, t: t}
	er.except = map[region]bool{}
	for _, reg := range except {
		er.except[reg] = true
	}
	return &er
}

func (er *exceptSectionReader) ReadAt(p []byte, offset int64) (int, error) {
	if er.except[region{offset, offset + int64(len(p)) - 1}] {
		er.t.Fatalf("Requested prohibited region of chunk: (%d, %d)", offset, offset+int64(len(p))-1)
	}
	return er.ra.ReadAt(p, offset)
}

func makeFile(t *testing.T, contents []byte, chunkSize int64) *file {
	testName := "test"
	sr := buildStargz(t, []tarent{
		regfile(testName, string(contents)),
	}, chunkSizeInfo(chunkSize))
	r, _, err := newReader(sr, &testCache{membuf: map[string]string{}, t: t})
	if err != nil {
		t.Fatalf("Failed to open stargz file: %v", err)
	}
	ra, err := r.OpenFile(testName)
	if err != nil {
		t.Fatalf("Failed to open testing file: %v", err)
	}
	f, ok := ra.(*file)
	if !ok {
		t.Fatalf("invalid type of file %q", testName)
	}
	return f
}

type tarent struct {
	header   *tar.Header
	contents []byte
}

func regfile(name string, contents string) tarent {
	if strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("file %q has suffix /", name))
	}
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0644,
			Size:     int64(len(contents)),
		},
		contents: []byte(contents),
	}
}

type chunkSizeInfo int

func buildStargz(t *testing.T, ents []tarent, opts ...interface{}) *io.SectionReader {
	var chunkSize chunkSizeInfo
	for _, opt := range opts {
		if v, ok := opt.(chunkSizeInfo); ok {
			chunkSize = v
		} else {
			t.Fatalf("unsupported opt")
		}
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, ent := range ents {
			if err := tw.WriteHeader(ent.header); err != nil {
				t.Errorf("writing header to the input tar: %v", err)
				pw.Close()
				return
			}
			if _, err := tw.Write(ent.contents); err != nil {
				t.Errorf("writing contents to the input tar: %v", err)
				pw.Close()
				return
			}
		}
		if err := tw.Close(); err != nil {
			t.Errorf("closing write of input tar: %v", err)
		}
		pw.Close()
	}()
	defer func() { go pr.Close(); go pw.Close() }()

	var stargzBuf bytes.Buffer
	w := stargz.NewWriter(&stargzBuf)
	if chunkSize > 0 {
		w.ChunkSize = int(chunkSize)
	}
	if err := w.AppendTar(pr); err != nil {
		t.Fatalf("failed to append tar file to stargz: %q", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close stargz writer: %q", err)
	}
	b := stargzBuf.Bytes()
	return io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b)))
}

func newReader(sr *io.SectionReader, cache cache.BlobCache) (*reader, *stargz.TOCEntry, error) {
	var r *reader
	gr, root, err := NewReader(sr, cache)
	if gr != nil {
		r = gr.(*reader)
	}
	return r, root, err
}
