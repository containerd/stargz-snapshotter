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
	"github.com/containerd/stargz-snapshotter/estargz"
	digest "github.com/opencontainers/go-digest"
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
	stargzFile, _ := buildStargz(t, []tarent{
		regfile(testFileName, sampleData1),
	}, chunkSizeInfo(sampleChunkSize))
	br := &breakReaderAt{
		ReaderAt: stargzFile,
		success:  true,
	}
	bev := &testTOCEntryVerifier{true}
	gr, _, err := newReader(io.NewSectionReader(br, 0, stargzFile.Size()), &nopCache{}, bev)
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

	for _, rs := range []bool{true, false} {
		for _, vs := range []bool{true, false} {
			br.success = rs
			bev.success = vs

			// tests for reading file
			p := make([]byte, len(sampleData1))
			n, err := fr.ReadAt(p, 0)
			if rs && vs {
				if err != nil || n != len(sampleData1) || !bytes.Equal([]byte(sampleData1), p) {
					t.Errorf("failed to read data but wanted to succeed: %v", err)
					return
				}
			} else {
				if err == nil {
					t.Errorf("succeeded to read data but wanted to fail (reader:%v,verify:%v)", rs, vs)
					return
				}
			}

			// tests for caching reader
			err = gr.Cache()
			if rs && vs {
				if err != nil {
					t.Errorf("failed to cache reader but wanted to succeed")
				}
			} else {
				if err == nil {
					t.Errorf("succeeded to cache reader but wanted to fail (reader:%v,verify:%v)", rs, vs)
				}
			}

		}
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

type testTOCEntryVerifier struct {
	success bool
}

func (bev *testTOCEntryVerifier) Verifier(ce *estargz.TOCEntry) (digest.Verifier, error) {
	return &testVerifier{bev.success}, nil
}

type testVerifier struct {
	success bool
}

func (bv *testVerifier) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (bv *testVerifier) Verified() bool {
	return bv.success
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
	sr, dgst := buildStargz(t, []tarent{
		regfile(testName, string(contents)),
	}, chunkSizeInfo(chunkSize))

	sgz, err := estargz.Open(sr)
	if err != nil {
		t.Fatalf("failed to parse converted stargz: %v", err)
	}
	ev, err := sgz.VerifyTOC(dgst)
	if err != nil {
		t.Fatalf("failed to verify stargz: %v", err)
	}

	r, _, err := newReader(sr, &testCache{membuf: map[string]string{}, t: t}, ev)
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

func buildStargz(t *testing.T, ents []tarent, opts ...interface{}) (*io.SectionReader, digest.Digest) {
	var chunkSize chunkSizeInfo
	for _, opt := range opts {
		if v, ok := opt.(chunkSizeInfo); ok {
			chunkSize = v
		} else {
			t.Fatalf("unsupported opt")
		}
	}

	tarBuf := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuf)
	for _, ent := range ents {
		if err := tw.WriteHeader(ent.header); err != nil {
			t.Fatalf("writing header to the input tar: %v", err)
		}
		if _, err := tw.Write(ent.contents); err != nil {
			t.Fatalf("writing contents to the input tar: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing write of input tar: %v", err)
	}

	tarData := tarBuf.Bytes()

	rc, dgst, err := estargz.Build(
		io.NewSectionReader(bytes.NewReader(tarData), 0, int64(len(tarData))),
		estargz.WithChunkSize(int(chunkSize)),
	)
	if err != nil {
		t.Fatalf("failed to build verifiable stargz: %v", err)
	}
	vsb := new(bytes.Buffer)
	if _, err := io.Copy(vsb, rc); err != nil {
		t.Fatalf("failed to copy built stargz blob: %v", err)
	}
	vsbb := vsb.Bytes()

	return io.NewSectionReader(bytes.NewReader(vsbb), 0, int64(len(vsbb))), dgst
}

func newReader(sr *io.SectionReader, cache cache.BlobCache, ev estargz.TOCEntryVerifier) (*reader, *estargz.TOCEntry, error) {
	var r *reader
	vr, root, err := NewReader(sr, cache)
	if vr != nil {
		r = vr.r
		r.verifier = ev
	}
	return r, root, err
}
