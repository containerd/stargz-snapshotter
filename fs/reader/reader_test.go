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
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
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
	stargzFile, _, err := testutil.BuildEStargz([]testutil.TarEntry{
		testutil.File(testFileName, sampleData1),
	}, testutil.WithEStargzOptions(estargz.WithChunkSize(sampleChunkSize)))
	if err != nil {
		t.Fatalf("failed to build sample estargz")
	}

	for _, rs := range []bool{true, false} {
		for _, vs := range []bool{true, false} {
			br := &breakReaderAt{
				ReaderAt: stargzFile,
				success:  true,
			}
			bev := &testTOCEntryVerifier{true}
			mcache := cache.NewMemoryCache()
			vr, gr, _, err := newReader(io.NewSectionReader(br, 0, stargzFile.Size()), mcache, bev)
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

			mcache.(*cache.MemoryCache).Membuf = map[string]*bytes.Buffer{}
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

			mcache.(*cache.MemoryCache).Membuf = map[string]*bytes.Buffer{}

			// tests for caching reader
			cacheErr := vr.Cache()
			verifyErr := vr.lastVerifyErr.Load()
			if rs {
				if vs {
					if !(cacheErr == nil && verifyErr == nil) {
						t.Errorf("failed to cache but wanted to succeed: %v; %v (reader:%v,verify:%v)", cacheErr, verifyErr, rs, vs)
					}
				} else {
					if !(cacheErr == nil && verifyErr != nil) {
						t.Errorf("cache reader: wanted: (cacheErr, verifyErr) = (nil, nonnil) but (%v,%v); (reader:%v,verify:%v)",
							cacheErr, verifyErr, rs, vs)
					}
				}
			} else {
				if !(cacheErr != nil && verifyErr != nil) {
					t.Errorf("cache reader: wanted: (cacheErr, verifyErr) = (nonnil, nonnil) but (%v,%v); (reader:%v,verify:%v)",
						cacheErr, verifyErr, rs, vs)
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
								id := genID(f.digest, reg.b, reg.e-reg.b+1)
								w, err := f.cache.Add(id)
								if err != nil {
									w.Close()
									t.Fatalf("failed to add cache %v: %v", id, err)
								}
								if _, err := w.Write([]byte(sampleData1[reg.b : reg.e+1])); err != nil {
									w.Close()
									t.Fatalf("failed to write cache %v: %v", id, err)
								}
								if err := w.Commit(); err != nil {
									w.Close()
									t.Fatalf("failed to commit cache %v: %v", id, err)
								}
								w.Close()
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
								id := genID(f.digest, ce.ChunkOffset, ce.ChunkSize)
								r, err := f.cache.Get(id)
								if err != nil {
									t.Errorf("missed cache of offset=%d, size=%d: %v(got size=%d)", ce.ChunkOffset, ce.ChunkSize, err, n)
									return
								}
								defer r.Close()
								if n, err := r.ReadAt(data, 0); (err != nil && err != io.EOF) || n != int(ce.ChunkSize) {
									t.Errorf("failed to read cache of offset=%d, size=%d: %v(got size=%d)", ce.ChunkOffset, ce.ChunkSize, err, n)
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

func makeFile(t *testing.T, contents []byte, chunkSize int) *file {
	testName := "test"
	sr, dgst, err := testutil.BuildEStargz([]testutil.TarEntry{
		testutil.File(testName, string(contents)),
	}, testutil.WithEStargzOptions(estargz.WithChunkSize(chunkSize)))
	if err != nil {
		t.Fatalf("failed to build sample estargz")
	}

	sgz, err := estargz.Open(sr)
	if err != nil {
		t.Fatalf("failed to parse converted stargz: %v", err)
	}
	ev, err := sgz.VerifyTOC(dgst)
	if err != nil {
		t.Fatalf("failed to verify stargz: %v", err)
	}

	_, r, _, err := newReader(sr, cache.NewMemoryCache(), ev)
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

func newReader(sr *io.SectionReader, cache cache.BlobCache, ev estargz.TOCEntryVerifier) (*VerifiableReader, *reader, *estargz.TOCEntry, error) {
	var r *reader
	telemetry := &estargz.Telemetry{}
	vr, err := NewReader(sr, cache, digest.FromString(""), telemetry)
	if vr != nil {
		r = vr.r
		vr.verifier = ev
		r.verifier = ev
	}
	root, ok := r.Lookup("")
	if !ok {
		return nil, nil, nil, fmt.Errorf("failed to get root")
	}
	return vr, r, root, err
}

func TestCacheVerify(t *testing.T) {
	stargzFile, tocDgst, err := testutil.BuildEStargz([]testutil.TarEntry{
		testutil.File("a", sampleData1+"a"),
		testutil.File("b", sampleData1+"b"),
	}, testutil.WithEStargzOptions(estargz.WithChunkSize(sampleChunkSize)))
	if err != nil {
		t.Fatalf("failed to build sample estargz")
	}
	for _, skipVerify := range [2]bool{true, false} {
		for _, invalidTOC := range [2]bool{true, false} {
			for _, invalidChunkBeforeVerify := range [2]bool{true, false} {
				for _, invalidChunkAfterVerify := range [2]bool{true, false} {
					name := fmt.Sprintf("test_cache_verify_%v_%v_%v_%v",
						skipVerify, invalidTOC, invalidChunkBeforeVerify, invalidChunkAfterVerify)
					t.Run(name, func(t *testing.T) {

						// Determine the expected behaviour
						var wantVerifyFail, wantCacheFail, wantCacheFail2 bool
						if skipVerify {
							// always no error if verification is disabled
							wantVerifyFail, wantCacheFail, wantCacheFail2 = false, false, false
						} else if invalidTOC || invalidChunkBeforeVerify {
							// errors occurred before verifying TOC must be reported via VerifyTOC()
							wantVerifyFail = true
						} else if invalidChunkAfterVerify {
							// errors occurred after verifying TOC must be reported via Cache()
							wantVerifyFail, wantCacheFail, wantCacheFail2 = false, true, true
						} else {
							// otherwise no verification error
							wantVerifyFail, wantCacheFail, wantCacheFail2 = false, false, false
						}

						// Prepare reader
						vr, err := NewReader(stargzFile, cache.NewMemoryCache(), digest.FromString(""), &estargz.Telemetry{})
						if err != nil {
							t.Fatalf("failed to prepare reader %v", err)
						}
						defer vr.Close()
						verifier := &failTOCEntryVerifier{}
						vr.verifier = verifier
						if invalidTOC {
							vr.verifier = nopTOCEntryVerifier{}
							vr.lastVerifyErr.Store(fmt.Errorf("invalidTOC"))
						}

						// Perform Cache() before verification
						// 1. Either of "a" or "b" is read and verified
						// 2. VerifyTOC/SkipVerify is called
						// 3. Another entry ("a" or "b") is called
						verifyDone := make(chan struct{})
						var firstEntryCalled bool
						var eg errgroup.Group
						eg.Go(func() error {
							return vr.Cache(WithFilter(func(ce *estargz.TOCEntry) bool {
								if ce.Name == "a" || ce.Name == "b" {
									if !firstEntryCalled {
										firstEntryCalled = true
										if invalidChunkBeforeVerify {
											verifier.registerFails([]string{ce.Name})
										}
										return true
									}
									<-verifyDone
									if invalidChunkAfterVerify {
										verifier.registerFails([]string{ce.Name})
									}
									return true
								}
								return false
							}))
						})
						time.Sleep(10 * time.Millisecond)

						// Perform verification
						if skipVerify {
							vr.SkipVerify()
						} else {
							_, err = vr.VerifyTOC(tocDgst)
						}
						if checkErr := checkError(wantVerifyFail, err); checkErr != nil {
							t.Errorf("verify: %v", checkErr)
							return
						}
						if err != nil {
							return
						}
						close(verifyDone)

						// Check the result of Cache()
						if checkErr := checkError(wantCacheFail, eg.Wait()); checkErr != nil {
							t.Errorf("cache: %v", checkErr)
							return
						}

						// Call Cache() again and check the result
						if checkErr := checkError(wantCacheFail2, vr.Cache()); checkErr != nil {
							t.Errorf("cache(2): %v", checkErr)
							return
						}
					})
				}
			}
		}
	}
}

type failTOCEntryVerifier struct {
	fails   []string
	failsMu sync.Mutex
}

func (f *failTOCEntryVerifier) registerFails(fails []string) {
	f.failsMu.Lock()
	defer f.failsMu.Unlock()
	f.fails = fails

}

func (f *failTOCEntryVerifier) Verifier(ce *estargz.TOCEntry) (digest.Verifier, error) {
	f.failsMu.Lock()
	defer f.failsMu.Unlock()
	success := true
	for _, n := range f.fails {
		if n == ce.Name {
			success = false
			break
		}
	}
	return &testVerifier{success}, nil
}

func checkError(wantFail bool, err error) error {
	if wantFail && err == nil {
		return fmt.Errorf("wanted to fail but succeeded")
	} else if !wantFail && err != nil {
		return errors.Wrapf(err, "wanted to succeed verification but failed")
	}
	return nil
}
