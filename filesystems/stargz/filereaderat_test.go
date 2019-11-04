package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"sync"
	"testing"

	"github.com/google/crfs/stargz"
)

const (
	tarHeaderSize      = 512
	sampleChunkSize    = 3
	sampleMiddleOffset = sampleChunkSize / 2
	sampleData1        = "0123456789"
	sampleData2        = "abcdefghij"
	lastChunkOffset1   = sampleChunkSize * (int64(len(sampleData1)) / sampleChunkSize)
	lastChunkOffset2   = sampleChunkSize * (int64(len(sampleData2)) / sampleChunkSize)
)

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
							f := makeFileReaderAt(t, []byte(sampleData1)[:filesize], sampleChunkSize)
							f.ra = newExceptSectionReader(t, f.ra, cacheExcept...)
							for _, reg := range cacheExcept {
								f.gr.cache.Add(f.gr.genID(f.name, reg.b, reg.e-reg.b+1), []byte(sampleData1[reg.b:reg.e+1]))
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
								ce, ok := f.gr.r.ChunkEntryForOffset(f.name, offset+int64(nr))
								if !ok {
									break
								}
								data, err := f.gr.cache.Fetch(f.gr.genID(ce.Name, ce.ChunkOffset, ce.ChunkSize))
								if err != nil || len(data) != int(ce.ChunkSize) {
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

func makeFileReaderAt(t *testing.T, contents []byte, chunkSize int64) *fileReaderAt {
	testName := "test"
	r, err := stargz.Open(makeStargz(t, []regEntry{
		{
			name:     testName,
			contents: string(contents),
		},
	}, sampleChunkSize))
	t.Logf("single entry stargz; contents=%q", string(contents))
	if err != nil {
		t.Fatalf("Failed to open stargz file: %v", err)
	}
	f, err := (&stargzReader{
		digest: "dummy",
		r:      r,
		cache:  &testCache{membuf: map[string]string{}, t: t},
	}).openFile(testName)
	if err != nil {
		t.Fatalf("Failed to open testing file: %v", err)
	}

	return f.(*fileReaderAt)
}

// Tests prefetch method of each stargz file.
func TestPrefetch(t *testing.T) {
	tests := []struct {
		name     string
		in       []regEntry
		fetchNum int      // number of chunks to fetch
		wantNum  int      // number of chunks wanted in the cache
		wants    []string // filenames to compare
	}{
		{
			name: "zero_region",
			in: []regEntry{
				{
					name:     "foo.txt",
					contents: sampleData1,
				},
			},
			fetchNum: 0,
			wantNum:  0,
		},
		{
			name: "smaller_region",
			in: []regEntry{
				{
					name:     "foo.txt",
					contents: sampleData1,
				},
			},
			fetchNum: 1 + chunkNum(sampleData1)/2, // header + file(partial)
			wantNum:  0,
		},
		{
			name: "just_file_region",
			in: []regEntry{
				{
					name:     "foo.txt",
					contents: sampleData1,
				},
			},
			fetchNum: 1 + chunkNum(sampleData1), // header + file(full)
			wantNum:  chunkNum(sampleData1),
			wants:    []string{"foo.txt"},
		},
		{
			name: "bigger_region",
			in: []regEntry{
				{
					name:     "foo.txt",
					contents: sampleData1,
				},
				{
					name:     "bar.txt",
					contents: sampleData2,
				},
			},
			fetchNum: 1 + chunkNum(sampleData1) + (chunkNum(sampleData2) / 2), // header + file(full) + file(partial)
			wantNum:  chunkNum(sampleData1),
			wants:    []string{"foo.txt"},
		},
		{
			name: "over_whole_stargz",
			in: []regEntry{
				{
					name:     "foo.txt",
					contents: sampleData1,
				},
			},
			fetchNum: 1 + chunkNum(sampleData1) + (chunkNum(sampleData1) / 2), // header + file(full) + file(partial)
			wantNum:  chunkNum(sampleData1),
			wants:    []string{"foo.txt"},
		},
		{
			name: "2_files",
			in: []regEntry{
				{
					name:     "foo.txt",
					contents: sampleData1,
				},
				{
					name:     "bar.txt",
					contents: sampleData2,
				},
			},
			fetchNum: 1 + chunkNum(sampleData1) + 1 + chunkNum(sampleData2), // header + file(full) + header + file(full)
			wantNum:  chunkNum(sampleData1) + chunkNum(sampleData2),
			wants:    []string{"foo.txt", "bar.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := makeStargz(t, tt.in, sampleChunkSize)
			r, err := stargz.Open(sr)
			if err != nil {
				t.Fatalf("Failed to open stargz file: %v", err)
			}
			gr := &stargzReader{
				digest: "test",
				r:      r,
				cache:  &testCache{membuf: map[string]string{}, t: t},
			}
			if err := gr.prefetch(sr, chunkNumToSize(t, tt.fetchNum, sr)); err != nil {
				t.Errorf("failed to prefetch: %v", err)
				return
			}
			if tt.wantNum != len(gr.cache.(*testCache).membuf) {
				t.Errorf("number of chunks in the cache %d; want %d", len(gr.cache.(*testCache).membuf), tt.wantNum)
				return
			}

			for _, file := range tt.wants {
				wantFile, err := gr.r.OpenFile(file)
				if err != nil {
					t.Fatalf("failed to open file %q: %v", file, err)
				}
				var nr int64
				for {
					ce, ok := gr.r.ChunkEntryForOffset(file, nr)
					if !ok {
						break
					}
					data, err := gr.cache.Fetch(gr.genID(ce.Name, ce.ChunkOffset, ce.ChunkSize))
					if err != nil {
						t.Errorf("failed to read cache data of %q: %v", file, err)
						return
					}
					wantData := make([]byte, ce.ChunkSize)
					wn, err := wantFile.ReadAt(wantData, ce.ChunkOffset)
					if err != nil {
						t.Errorf("failed to read want data of %q: %v", file, err)
						return
					}
					if len(data) != wn {
						t.Errorf("size of cached data %d; want %d", len(data), wn)
						return
					}
					if bytes.Compare(data, wantData) != 0 {
						t.Errorf("cached data %q; want %q", string(data), string(wantData))
						return
					}

					nr += ce.ChunkSize
				}
			}
		})
	}
}

func chunkNum(data string) int {
	return (len(data)-1)/sampleChunkSize + 1
}

type byteReader struct {
	io.Reader
}

func (br *byteReader) ReadByte() (byte, error) {
	var b [1]byte
	if n, _ := br.Read(b[:]); n > 0 {
		return b[0], nil
	}
	return 0, io.EOF
}

func chunkNumToSize(t *testing.T, fetchNum int, sr *io.SectionReader) int64 {
	br := &byteReader{sr}
	zr, err := gzip.NewReader(br)
	if err != nil {
		t.Fatalf("failed to get gzip reader for original data: %v", err)
	}

	nc := 0
	for {
		if nc >= fetchNum {
			break
		}
		zr.Multistream(false)
		if _, err := ioutil.ReadAll(zr); err != nil {
			t.Fatalf("failed to read gzip stream: %v", err)
		}
		if err := zr.Reset(br); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("failed to reset gzip stream: %v", err)
		}
		nc++
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("failed to close gzip stream: %v", err)
	}

	// get current position
	rn, err := sr.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("failed to seek to current position to get size: %v", err)
	}

	return rn
}

type regEntry struct {
	name     string
	contents string
}

func makeStargz(t *testing.T, ents []regEntry, chunkSize int64) *io.SectionReader {
	// builds a sample stargz
	tr, cancel := buildTar(t, ents)
	defer cancel()
	var stargzBuf bytes.Buffer
	w := stargz.NewWriter(&stargzBuf)
	w.ChunkSize = int(chunkSize)
	if err := w.AppendTar(tr); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}
	stargzData, err := ioutil.ReadAll(&stargzBuf)
	if err != nil {
		t.Fatalf("Read all stargz data: %v", err)
	}

	// opens the sample stargz
	return io.NewSectionReader(bytes.NewReader(stargzData), 0, int64(len(stargzData)))
}

func buildTar(t *testing.T, ents []regEntry) (r io.Reader, cancel func()) {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, ent := range ents {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     ent.name,
				Mode:     0644,
				Size:     int64(len(ent.contents)),
			}); err != nil {
				t.Errorf("writing header to the input tar: %v", err)
				pw.Close()
				return
			}
			if _, err := tw.Write([]byte(ent.contents)); err != nil {
				t.Errorf("writing contents to the input tar: %v", err)
				pw.Close()
				return
			}
		}
		if err := tw.Close(); err != nil {
			t.Errorf("closing write of input tar: %v", err)
		}
		pw.Close()
		return
	}()
	return pr, func() { go pr.Close(); go pw.Close() }
}

type testCache struct {
	membuf map[string]string
	t      *testing.T
	mu     sync.Mutex
}

func (tc *testCache) Fetch(blobHash string) ([]byte, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	cache, ok := tc.membuf[blobHash]
	if !ok {
		return nil, fmt.Errorf("Missed cache: %q", blobHash)
	}
	return []byte(cache), nil
}

func (tc *testCache) Add(blobHash string, p []byte) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.membuf[blobHash] = string(p)
	tc.t.Logf("  cached [%s...]: %q", blobHash[:8], string(p))

	return
}
