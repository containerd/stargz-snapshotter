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
   license that can be found in the LICENSE file.
*/

package estargz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

func TestSort(t *testing.T) {
	longname1 := longstring(120)
	longname2 := longstring(150)

	tests := []struct {
		name             string
		in               []tarEntry
		log              []string // MUST NOT include "./" prefix here
		want             []tarEntry
		wantFail         bool
		allowMissedFiles []string
	}{
		{
			name: "nolog",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				file("bar/bar.txt", "bar"),
			),
			want: tarOf(
				noPrefetchLandmark(),
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				file("bar/bar.txt", "bar"),
			),
		},
		{
			name: "identical",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
			),
			log: []string{"foo.txt", "bar/baz.txt"},
			want: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				prefetchLandmark(),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
			),
		},
		{
			name: "shuffle_reg",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				file("bar.txt", "bar"),
				file("baa.txt", "baa"),
			),
			log: []string{"baa.txt", "bar.txt", "baz.txt"},
			want: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "shuffle_directory",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
				dir("baz/"),
				file("baz/baz1.txt", "baz"),
				file("baz/baz2.txt", "baz"),
				dir("baz/bazbaz/"),
				file("baz/bazbaz/bazbaz_b.txt", "baz"),
				file("baz/bazbaz/bazbaz_a.txt", "baz"),
			),
			log: []string{"baz/bazbaz/bazbaz_a.txt", "baz/baz2.txt", "foo.txt"},
			want: tarOf(
				dir("baz/"),
				dir("baz/bazbaz/"),
				file("baz/bazbaz/bazbaz_a.txt", "baz"),
				file("baz/baz2.txt", "baz"),
				file("foo.txt", "foo"),
				prefetchLandmark(),
				dir("bar/"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
				file("baz/baz1.txt", "baz"),
				file("baz/bazbaz/bazbaz_b.txt", "baz"),
			),
		},
		{
			name: "shuffle_link",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				link("bar.txt", "baz.txt"),
				file("baa.txt", "baa"),
			),
			log: []string{"baz.txt"},
			want: tarOf(
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
				link("bar.txt", "baz.txt"),
				file("baa.txt", "baa"),
			),
		},
		{
			name: "longname",
			in: tarOf(
				file("foo.txt", "foo"),
				file(longname1, "test"),
				dir("bar/"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
				file(fmt.Sprintf("bar/%s", longname2), "test2"),
			),
			log: []string{fmt.Sprintf("bar/%s", longname2), longname1},
			want: tarOf(
				dir("bar/"),
				file(fmt.Sprintf("bar/%s", longname2), "test2"),
				file(longname1, "test"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
				file("bar/bar.txt", "bar"),
				file("bar/baa.txt", "baa"),
			),
		},
		{
			name: "various_types",
			in: tarOf(
				file("foo.txt", "foo"),
				symlink("foo2", "foo.txt"),
				chardev("foochar", 10, 50),
				blockdev("fooblock", 15, 20),
				fifo("fifoo"),
			),
			log: []string{"fifoo", "foo2", "foo.txt", "fooblock"},
			want: tarOf(
				fifo("fifoo"),
				symlink("foo2", "foo.txt"),
				file("foo.txt", "foo"),
				blockdev("fooblock", 15, 20),
				prefetchLandmark(),
				chardev("foochar", 10, 50),
			),
		},
		{
			name: "existing_landmark",
			in: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
			log: []string{"foo.txt", "bar.txt"},
			want: tarOf(
				file("foo.txt", "foo"),
				file("bar.txt", "bar"),
				prefetchLandmark(),
				file("baa.txt", "baa"),
				file("baz.txt", "baz"),
			),
		},
		{
			name: "existing_landmark_nolog",
			in: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
			want: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "existing_noprefetch_landmark",
			in: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
			log: []string{"foo.txt", "bar.txt"},
			want: tarOf(
				file("foo.txt", "foo"),
				file("bar.txt", "bar"),
				prefetchLandmark(),
				file("baa.txt", "baa"),
				file("baz.txt", "baz"),
			),
		},
		{
			name: "existing_noprefetch_landmark_nolog",
			in: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
			want: tarOf(
				noPrefetchLandmark(),
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				file("baz.txt", "baz"),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "not_existing_file",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				file("bar.txt", "bar"),
				file("baa.txt", "baa"),
			),
			log:      []string{"baa.txt", "bar.txt", "dummy"},
			want:     tarOf(),
			wantFail: true,
		},
		{
			name: "not_existing_file_allow_fail",
			in: tarOf(
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
				file("bar.txt", "bar"),
				file("baa.txt", "baa"),
			),
			log: []string{"baa.txt", "dummy1", "bar.txt", "dummy2"},
			want: tarOf(
				file("baa.txt", "baa"),
				file("bar.txt", "bar"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
				file("baz.txt", "baz"),
			),
			allowMissedFiles: []string{"dummy1", "dummy2"},
		},
		{
			name: "duplicated_entry",
			in: tarOf(
				file("foo.txt", "foo"),
				dir("bar/"),
				file("bar/baz.txt", "baz"),
				dir("bar/"),
				file("bar/baz.txt", "baz2"),
			),
			log: []string{"bar/baz.txt"},
			want: tarOf(
				dir("bar/"),
				file("bar/baz.txt", "baz2"),
				prefetchLandmark(),
				file("foo.txt", "foo"),
			),
		},
		{
			name: "hardlink",
			in: tarOf(
				file("baz.txt", "aaaaa"),
				link("bazlink", "baz.txt"),
			),
			log: []string{"bazlink"},
			want: tarOf(
				file("baz.txt", "aaaaa"),
				link("bazlink", "baz.txt"),
				prefetchLandmark(),
			),
		},
		{
			name: "root_relative_file",
			in: tarOf(
				dir("./"),
				file("./foo.txt", "foo"),
				file("./baz.txt", "baz"),
				file("./baa.txt", "baa"),
				link("./bazlink", "./baz.txt"),
			),
			log: []string{"baa.txt", "bazlink"},
			want: tarOf(
				dir("./"),
				file("./baa.txt", "baa"),
				file("./baz.txt", "baz"),
				link("./bazlink", "./baz.txt"),
				prefetchLandmark(),
				file("./foo.txt", "foo"),
			),
		},
		{
			// GNU tar accepts tar containing absolute path
			name: "root_absolute_file",
			in: tarOf(
				dir("/"),
				file("/foo.txt", "foo"),
				file("/baz.txt", "baz"),
				file("/baa.txt", "baa"),
				link("/bazlink", "/baz.txt"),
			),
			log: []string{"baa.txt", "bazlink"},
			want: tarOf(
				dir("/"),
				file("/baa.txt", "baa"),
				file("/baz.txt", "baz"),
				link("/bazlink", "/baz.txt"),
				prefetchLandmark(),
				file("/foo.txt", "foo"),
			),
		},
	}
	for _, tt := range tests {
		for _, srcCompression := range srcCompressions {
			for _, logprefix := range allowedPrefix {
				for _, tarprefix := range allowedPrefix {
					t.Run(fmt.Sprintf("%s-logprefix=%q-tarprefix=%q-src=%d", tt.name, logprefix, tarprefix, srcCompression), func(t *testing.T) {
						// Sort tar file
						var pfiles []string
						for _, f := range tt.log {
							pfiles = append(pfiles, logprefix+f)
						}
						var opts []Option
						var missedFiles []string
						if tt.allowMissedFiles != nil {
							opts = append(opts, WithAllowPrioritizeNotFound(&missedFiles))
						}
						rc, err := Build(compressBlob(t, buildTar(t, tt.in, tarprefix), srcCompression),
							append(opts, WithPrioritizedFiles(pfiles))...)
						if tt.wantFail {
							if err != nil {
								return
							}
							t.Errorf("This test must fail but succeeded")
							return
						}
						if err != nil {
							t.Errorf("failed to build stargz: %v", err)
						}
						defer rc.Close()
						zr, err := gzip.NewReader(rc)
						if err != nil {
							t.Fatalf("failed to create gzip reader: %v", err)
						}
						if tt.allowMissedFiles != nil {
							want := map[string]struct{}{}
							for _, f := range tt.allowMissedFiles {
								want[logprefix+f] = struct{}{}
							}
							got := map[string]struct{}{}
							for _, f := range missedFiles {
								got[f] = struct{}{}
							}
							if !reflect.DeepEqual(got, want) {
								t.Errorf("unexpected missed files: want %v, got: %v",
									want, got)
								return
							}
						}
						gotTar := tar.NewReader(zr)

						// Compare all
						wantTar := tar.NewReader(buildTar(t, tt.want, tarprefix))
						for {
							// Fetch and parse next header.
							gotH, wantH, err := next(t, gotTar, wantTar)
							if err != nil {
								if err == io.EOF {
									break
								}
								t.Fatalf("Failed to parse tar file: %v", err)
							}

							if !reflect.DeepEqual(gotH, wantH) {
								t.Errorf("different header (got = name:%q,type:%d,size:%d; want = name:%q,type:%d,size:%d)",
									gotH.Name, gotH.Typeflag, gotH.Size, wantH.Name, wantH.Typeflag, wantH.Size)
								return

							}

							got, err := io.ReadAll(gotTar)
							if err != nil {
								t.Fatal("failed to read got tar payload")
							}
							want, err := io.ReadAll(wantTar)
							if err != nil {
								t.Fatal("failed to read want tar payload")
							}
							if !bytes.Equal(got, want) {
								t.Errorf("different payload (got = %q; want = %q)", string(got), string(want))
								return
							}
						}
					})
				}
			}
		}
	}
}

func next(t *testing.T, a *tar.Reader, b *tar.Reader) (ah *tar.Header, bh *tar.Header, err error) {
	eofA, eofB := false, false

	ah, err = nextWithSkipTOC(a)
	if err != nil {
		if err == io.EOF {
			eofA = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	bh, err = nextWithSkipTOC(b)
	if err != nil {
		if err == io.EOF {
			eofB = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	if eofA != eofB {
		if !eofA {
			t.Logf("a = %q", ah.Name)
		}
		if !eofB {
			t.Logf("b = %q", bh.Name)
		}
		t.Fatalf("got eof %t != %t", eofB, eofA)
	}
	if eofA {
		err = io.EOF
	}

	return
}

func nextWithSkipTOC(a *tar.Reader) (h *tar.Header, err error) {
	if h, err = a.Next(); err != nil {
		return
	}
	if h.Name == TOCTarName {
		return nextWithSkipTOC(a)
	}
	return
}

func longstring(size int) (str string) {
	unit := "long"
	for i := 0; i < size/len(unit)+1; i++ {
		str = fmt.Sprintf("%s%s", str, unit)
	}

	return str[:size]
}

func TestCountReader(t *testing.T) {
	tests := []struct {
		name    string
		ops     func(*countReadSeeker) error
		wantPos int64
	}{
		{
			name: "nop",
			ops: func(pw *countReadSeeker) error {
				return nil
			},
			wantPos: 0,
		},
		{
			name: "read",
			ops: func(pw *countReadSeeker) error {
				size := 5
				if _, err := pw.Read(make([]byte, size)); err != nil {
					return err
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "readtwice",
			ops: func(pw *countReadSeeker) error {
				size1, size2 := 5, 3
				if _, err := pw.Read(make([]byte, size1)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Read(make([]byte, size2)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 8,
		},
		{
			name: "seek_start",
			ops: func(pw *countReadSeeker) error {
				size := int64(5)
				if _, err := pw.Seek(size, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "seek_start_twice",
			ops: func(pw *countReadSeeker) error {
				size1, size2 := int64(5), int64(3)
				if _, err := pw.Seek(size1, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 3,
		},
		{
			name: "seek_current",
			ops: func(pw *countReadSeeker) error {
				size := int64(5)
				if _, err := pw.Seek(size, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 5,
		},
		{
			name: "seek_current_twice",
			ops: func(pw *countReadSeeker) error {
				size1, size2 := int64(5), int64(3)
				if _, err := pw.Seek(size1, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 8,
		},
		{
			name: "seek_current_twice_negative",
			ops: func(pw *countReadSeeker) error {
				size1, size2 := int64(5), int64(-3)
				if _, err := pw.Seek(size1, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 2,
		},
		{
			name: "mixed",
			ops: func(pw *countReadSeeker) error {
				size1, size2, size3, size4, size5 := int64(5), int64(-3), int64(4), int64(-1), int64(6)
				if _, err := pw.Read(make([]byte, size1)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size2, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size3, io.SeekStart); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Seek(size4, io.SeekCurrent); err != nil {
					if err != io.EOF {
						return err
					}
				}
				if _, err := pw.Read(make([]byte, size5)); err != nil {
					if err != io.EOF {
						return err
					}
				}
				return nil
			},
			wantPos: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pw, err := newCountReadSeeker(bytes.NewReader(make([]byte, 100)))
			if err != nil {
				t.Fatalf("failed to make position watcher: %q", err)
			}

			if err := tt.ops(pw); err != nil {
				t.Fatalf("failed to operate position watcher: %q", err)
			}

			gotPos := pw.currentPos()
			if tt.wantPos != gotPos {
				t.Errorf("current position: %d; want %d", gotPos, tt.wantPos)
			}
		})
	}

}

// incompressibleContent returns deterministic, hard-to-compress bytes so that
// compressed size tracks uncompressed size, letting minChunkSize tests
// predictably cross stream boundaries.
func incompressibleContent(seed uint64, n int) string {
	b := make([]byte, n)
	x := seed*2862933555777941757 + 3037000493
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = byte(33 + (x>>33)%94) // printable ASCII
	}
	return string(b)
}

func buildBlobBytes(t *testing.T, tarBytes []byte, opts ...Option) []byte {
	t.Helper()
	blob, err := Build(io.NewSectionReader(bytes.NewReader(tarBytes), 0, int64(len(tarBytes))), opts...)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, err := io.ReadAll(blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if err := blob.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	return data
}

// streamOffsets returns the distinct gzip-stream start offsets in a built blob.
func streamOffsets(t *testing.T, blob []byte) map[int64]struct{} {
	t.Helper()
	r, err := Open(io.NewSectionReader(bytes.NewReader(blob), 0, int64(len(blob))))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	offsets := make(map[int64]struct{})
	for _, e := range r.toc.Entries {
		if e.Type == "chunk" || (e.Type == "reg" && e.Size > 0) {
			offsets[e.Offset] = struct{}{}
		}
	}
	return offsets
}

// sortedStreamOffsets returns the distinct gzip-stream start offsets in
// ascending order, so consecutive differences give each interior stream's size.
func sortedStreamOffsets(t *testing.T, blob []byte) []int64 {
	t.Helper()
	set := streamOffsets(t, blob)
	out := make([]int64, 0, len(set))
	for o := range set {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// assertRoundTrip verifies that every file in want is recoverable from blob.
func assertRoundTrip(t *testing.T, blob []byte, want map[string]string) {
	t.Helper()
	r, err := Open(io.NewSectionReader(bytes.NewReader(blob), 0, int64(len(blob))))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for name, content := range want {
		e, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("missing entry %q", name)
		}
		sr, err := r.OpenFile(name)
		if err != nil {
			t.Fatalf("OpenFile %q: %v", name, err)
		}
		got := make([]byte, e.Size)
		if _, err := sr.ReadAt(got, 0); err != nil && err != io.EOF {
			t.Fatalf("ReadAt %q: %v", name, err)
		}
		if string(got) != content {
			t.Fatalf("content mismatch for %q", name)
		}
	}
}

// compressibleContent returns n bytes from a short repeating pattern so a
// worker's slice compresses well, keeping parallel MinChunkSize test blobs small.
func compressibleContent(seed uint64, n int) string {
	const pat = "estargz/parallel-min-chunk-size/0123456789abcdef "
	b := make([]byte, n)
	off := int(seed) % len(pat)
	for i := range b {
		b[i] = pat[(off+i)%len(pat)]
	}
	return string(b)
}

func sampleTar(t *testing.T, n, size int, gen func(seed uint64, n int) string) (tarBytes []byte, want map[string]string) {
	t.Helper()
	want = make(map[string]string, n)
	var ents []tarEntry
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file%04d", i)
		content := gen(uint64(i), size)
		want[name] = content
		ents = append(ents, file(name, content))
	}
	sr := buildTar(t, ents, "")
	tarBytes = make([]byte, sr.Size())
	if _, err := sr.ReadAt(tarBytes, 0); err != nil && err != io.EOF {
		t.Fatalf("read tar: %v", err)
	}
	return tarBytes, want
}

func entriesOfSizes(sizes ...int64) []*entry {
	es := make([]*entry, len(sizes))
	for i, s := range sizes {
		es[i] = &entry{header: &tar.Header{Size: s}}
	}
	return es
}

// TestDivideEntriesByMinSize verifies that every group but the last carries at
// least the per-worker target and the group count never exceeds the worker cap.
func TestDivideEntriesByMinSize(t *testing.T) {
	tests := []struct {
		name        string
		sizes       []int64
		maxParts    int
		minPartSize int64
		wantParts   int
	}{
		{"fills exactly maxParts", []int64{100, 100, 100, 100, 100, 100, 100, 100}, 4, 100, 4},
		{"floor raises target", []int64{100, 100, 100, 100, 100, 100, 100, 100}, 100, 250, 2},
		{"cap raises target", []int64{100, 100, 100, 100, 100, 100, 100, 100}, 2, 100, 2},
		{"single huge entry", []int64{1000}, 4, 100, 1},
		{"skewed entries", []int64{100, 1000, 100, 100}, 4, 150, 2},
		{"oversized maxParts", []int64{100, 100, 100, 100, 100, 100, 100, 100}, 1_000_000, 250, 2},
		{"trailing runt folds into last group", []int64{100, 100, 100, 100, 100}, 4, 150, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := entriesOfSizes(tt.sizes...)
			var total int64
			for _, s := range tt.sizes {
				total += s
			}
			target := total / int64(tt.maxParts)
			if target < tt.minPartSize {
				target = tt.minPartSize
			}

			set := divideEntriesByMinSize(entries, tt.maxParts, tt.minPartSize)

			if len(set) > tt.maxParts {
				t.Fatalf("group count %d exceeds maxParts %d", len(set), tt.maxParts)
			}
			if tt.wantParts != 0 && len(set) != tt.wantParts {
				t.Fatalf("group count = %d, want %d", len(set), tt.wantParts)
			}
			// Every group but the last must reach the target.
			for i := 0; i+1 < len(set); i++ {
				var sum int64
				for _, e := range set[i] {
					sum += e.header.Size
				}
				if sum < target {
					t.Fatalf("group %d carries %d < target %d", i, sum, target)
				}
			}
			// The last group must also reach minPartSize when there are multiple groups.
			if len(set) > 1 {
				var sum int64
				for _, e := range set[len(set)-1] {
					sum += e.header.Size
				}
				if sum < tt.minPartSize {
					t.Fatalf("last group carries %d < minPartSize %d", sum, tt.minPartSize)
				}
			}
			// The partition must preserve every entry in order.
			var flat []*entry
			for _, g := range set {
				flat = append(flat, g...)
			}
			if !reflect.DeepEqual(flat, entries) {
				t.Fatalf("partition did not preserve entries in order")
			}
		})
	}
}

// buildAtGOMAXPROCS builds tarBytes at the given GOMAXPROCS, then restores it.
func buildAtGOMAXPROCS(t *testing.T, procs int, tarBytes []byte, opts ...Option) []byte {
	t.Helper()
	prev := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(prev)
	return buildBlobBytes(t, tarBytes, opts...)
}

// TestBuildMinChunkSizeMergesStreams exercises the per-worker merge end to end:
// every stream (including the final one) must reach MinChunkSize.
func TestBuildMinChunkSizeMergesStreams(t *testing.T) {
	const (
		minChunk = 128
		workers  = 2
	)
	// Each worker must hold minChunk*maxGzipCompressionRatio incompressible bytes;
	// size the layer to fill both workers.
	perWorker := minChunk * maxGzipCompressionRatio
	tarBytes, want := sampleTar(t, workers*5, perWorker/4, incompressibleContent)

	blob := buildAtGOMAXPROCS(t, workers, tarBytes, WithChunkSize(512), WithMinChunkSize(minChunk))
	assertRoundTrip(t, blob, want)

	offsets := sortedStreamOffsets(t, blob)
	if len(offsets) <= workers {
		t.Fatalf("expected several streams per worker, got %d streams for %d workers", len(offsets), workers)
	}
	// Interior streams.
	for i := 0; i+1 < len(offsets); i++ {
		if size := offsets[i+1] - offsets[i]; size < minChunk {
			t.Fatalf("interior stream at offset %d is under-packed: %d < %d", offsets[i], size, minChunk)
		}
	}
	// Final stream: from last offset to the start of the TOC.
	tocOff, _, err := OpenFooter(io.NewSectionReader(bytes.NewReader(blob), 0, int64(len(blob))))
	if err != nil {
		t.Fatalf("OpenFooter: %v", err)
	}
	if size := tocOff - offsets[len(offsets)-1]; size < minChunk {
		t.Fatalf("final stream at offset %d is under-packed: %d < %d", offsets[len(offsets)-1], size, minChunk)
	}
}

// TestBuildMinChunkSizeParallelizes checks that a large layer splits across
// workers while a small layer stays byte-identical to the single-core build.
func TestBuildMinChunkSizeParallelizes(t *testing.T) {
	const minChunk = 512
	perWorker := minChunk * maxGzipCompressionRatio

	t.Run("large layer splits", func(t *testing.T) {
		tarBytes, want := sampleTar(t, 64, perWorker/8, compressibleContent) // several workers' worth
		parallel := buildAtGOMAXPROCS(t, 4, tarBytes, WithMinChunkSize(minChunk))
		sequential := buildAtGOMAXPROCS(t, 1, tarBytes, WithMinChunkSize(minChunk))

		assertRoundTrip(t, parallel, want)
		if bytes.Equal(parallel, sequential) {
			t.Fatalf("a large layer should be split across workers")
		}
	})

	t.Run("small layer stays sequential", func(t *testing.T) {
		// Far below one worker's floor: the build is single-part at any GOMAXPROCS,
		// so parallel and sequential must be byte-identical.
		var ents []tarEntry
		want := map[string]string{}
		for i := 0; i < 4; i++ {
			name := fmt.Sprintf("full%02d", i)
			c := incompressibleContent(uint64(i), 4*minChunk) // one full stream each
			ents = append(ents, file(name, c))
			want[name] = c
		}
		tail := incompressibleContent(99, 32) // short trailing stream
		ents = append(ents, file("tail", tail))
		want["tail"] = tail

		sr := buildTar(t, ents, "")
		tarBytes := make([]byte, sr.Size())
		if _, err := sr.ReadAt(tarBytes, 0); err != nil && err != io.EOF {
			t.Fatalf("read tar: %v", err)
		}

		parallel := buildAtGOMAXPROCS(t, 4, tarBytes, WithMinChunkSize(minChunk))
		sequential := buildAtGOMAXPROCS(t, 1, tarBytes, WithMinChunkSize(minChunk))

		assertRoundTrip(t, parallel, want)
		if !bytes.Equal(parallel, sequential) {
			t.Fatalf("a single-part build must be GOMAXPROCS-independent; parallel != sequential")
		}
	})
}

// TestBuildMinChunkSizeFoldsTail ensures the writer folds a short trailing
// stream into its predecessor so no stream falls below MinChunkSize.
func TestBuildMinChunkSizeFoldsTail(t *testing.T) {
	const minChunk = 4096
	var ents []tarEntry
	want := map[string]string{}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("full%02d", i)
		c := incompressibleContent(uint64(i), 4*minChunk)
		ents = append(ents, file(name, c))
		want[name] = c
	}
	tail := incompressibleContent(99, 32)
	ents = append(ents, file("tail", tail))
	want["tail"] = tail

	sr := buildTar(t, ents, "")
	tarBytes := make([]byte, sr.Size())
	if _, err := sr.ReadAt(tarBytes, 0); err != nil && err != io.EOF {
		t.Fatalf("read tar: %v", err)
	}

	blob := buildAtGOMAXPROCS(t, 1, tarBytes, WithMinChunkSize(minChunk))
	assertRoundTrip(t, blob, want)

	offsets := sortedStreamOffsets(t, blob)
	if len(offsets) < 2 {
		t.Fatalf("expected multiple streams, got %d", len(offsets))
	}
	tocOff, _, err := OpenFooter(io.NewSectionReader(bytes.NewReader(blob), 0, int64(len(blob))))
	if err != nil {
		t.Fatalf("OpenFooter: %v", err)
	}
	ends := append(offsets[1:], tocOff)
	for i, off := range offsets {
		if size := ends[i] - off; size < minChunk {
			t.Fatalf("stream at offset %d is under-packed: %d < %d", off, size, minChunk)
		}
	}
}

// TestBuildMinChunkSizeKeepsLandmarkStream ensures the prefetch landmark keeps
// its own stream even when that stream ends below MinChunkSize: its offset
// marks the end of the prefetch range, so folding it backward would shrink the
// range to the preceding stream's start.
func TestBuildMinChunkSizeKeepsLandmarkStream(t *testing.T) {
	const minChunk = 4096
	var ents []tarEntry
	var prioritized []string
	want := map[string]string{}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("full%02d", i)
		c := incompressibleContent(uint64(i), 4*minChunk)
		ents = append(ents, file(name, c))
		want[name] = c
		prioritized = append(prioritized, name)
	}

	sr := buildTar(t, ents, "")
	tarBytes := make([]byte, sr.Size())
	if _, err := sr.ReadAt(tarBytes, 0); err != nil && err != io.EOF {
		t.Fatalf("read tar: %v", err)
	}

	// All files prioritized: the landmark is the last entry, ending the blob as
	// a short stream that must not be folded.
	blob := buildAtGOMAXPROCS(t, 1, tarBytes,
		WithMinChunkSize(minChunk), WithPrioritizedFiles(prioritized))
	assertRoundTrip(t, blob, want)

	r, err := Open(io.NewSectionReader(bytes.NewReader(blob), 0, int64(len(blob))))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	landmark, ok := r.Lookup(PrefetchLandmark)
	if !ok {
		t.Fatalf("missing prefetch landmark")
	}
	if landmark.InnerOffset != 0 {
		t.Fatalf("landmark was folded into the preceding stream (InnerOffset = %d)", landmark.InnerOffset)
	}
	last, ok := r.Lookup(fmt.Sprintf("full%02d", 3))
	if !ok {
		t.Fatalf("missing last file")
	}
	if landmark.Offset <= last.Offset {
		t.Fatalf("landmark offset %d does not end the prefetch range (last file at %d)", landmark.Offset, last.Offset)
	}
}
