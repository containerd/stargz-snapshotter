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
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const (
	uncompressedType int = iota
	gzipType
	zstdType
)

var srcCompressions = []int{
	uncompressedType,
	gzipType,
	zstdType,
}

// TestBuild tests the resulting stargz blob built by this pkg has the same
// contents as the normal stargz blob.
func TestBuild(t *testing.T) {
	tests := []struct {
		name      string
		chunkSize int
		in        []tarEntry
	}{
		{
			name:      "regfiles and directories",
			chunkSize: 4,
			in: tarOf(
				file("foo", "test1"),
				dir("foo2/"),
				file("foo2/bar", "test2", xAttr(map[string]string{"test": "sample"})),
			),
		},
		{
			name:      "empty files",
			chunkSize: 4,
			in: tarOf(
				file("foo", "tttttt"),
				file("foo_empty", ""),
				file("foo2", "tttttt"),
				file("foo_empty2", ""),
				file("foo3", "tttttt"),
				file("foo_empty3", ""),
				file("foo4", "tttttt"),
				file("foo_empty4", ""),
				file("foo5", "tttttt"),
				file("foo_empty5", ""),
				file("foo6", "tttttt"),
			),
		},
		{
			name:      "various files",
			chunkSize: 4,
			in: tarOf(
				file("baz.txt", "bazbazbazbazbazbazbaz"),
				file("foo.txt", "a"),
				symlink("barlink", "test/bar.txt"),
				dir("test/"),
				dir("dev/"),
				blockdev("dev/testblock", 3, 4),
				fifo("dev/testfifo"),
				chardev("dev/testchar1", 5, 6),
				file("test/bar.txt", "testbartestbar", xAttr(map[string]string{"test2": "sample2"})),
				dir("test2/"),
				link("test2/bazlink", "baz.txt"),
				chardev("dev/testchar2", 1, 2),
			),
		},
		{
			name:      "no contents",
			chunkSize: 4,
			in: tarOf(
				file("baz.txt", ""),
				symlink("barlink", "test/bar.txt"),
				dir("test/"),
				dir("dev/"),
				blockdev("dev/testblock", 3, 4),
				fifo("dev/testfifo"),
				chardev("dev/testchar1", 5, 6),
				file("test/bar.txt", "", xAttr(map[string]string{"test2": "sample2"})),
				dir("test2/"),
				link("test2/bazlink", "baz.txt"),
				chardev("dev/testchar2", 1, 2),
			),
		},
	}
	for _, tt := range tests {
		for _, cl := range compressionLevels {
			cl := cl
			for _, srcCompression := range srcCompressions {
				srcCompression := srcCompression
				for _, prefix := range allowedPrefix {
					prefix := prefix
					t.Run(tt.name+"-"+fmt.Sprintf("compression=%v-prefix=%q", cl, prefix), func(t *testing.T) {

						tarBlob := buildTarStatic(t, tt.in, prefix)
						// Test divideEntries()
						entries, err := sortEntries(tarBlob, nil, nil) // identical order
						if err != nil {
							t.Fatalf("faield to parse tar: %v", err)
						}
						var merged []*entry
						for _, part := range divideEntries(entries, 4) {
							merged = append(merged, part...)
						}
						if !reflect.DeepEqual(entries, merged) {
							for _, e := range entries {
								t.Logf("Original: %v", e.header)
							}
							for _, e := range merged {
								t.Logf("Merged: %v", e.header)
							}
							t.Errorf("divided entries couldn't be merged")
							return
						}

						// Prepare sample data
						wantBuf := new(bytes.Buffer)
						sw := NewWriterLevel(wantBuf, cl)
						sw.ChunkSize = tt.chunkSize
						if err := sw.AppendTar(tarBlob); err != nil {
							t.Fatalf("faield to append tar to want stargz: %v", err)
						}
						if _, err := sw.Close(); err != nil {
							t.Fatalf("faield to prepare want stargz: %v", err)
						}
						wantData := wantBuf.Bytes()
						want, err := Open(io.NewSectionReader(
							bytes.NewReader(wantData), 0, int64(len(wantData))))
						if err != nil {
							t.Fatalf("failed to parse the want stargz: %v", err)
						}

						// Prepare testing data
						rc, err := Build(compressBlob(t, tarBlob, srcCompression), WithChunkSize(tt.chunkSize), WithCompressionLevel(cl))
						if err != nil {
							t.Fatalf("faield to build stargz: %v", err)
						}
						defer rc.Close()
						gotBuf := new(bytes.Buffer)
						if _, err := io.Copy(gotBuf, rc); err != nil {
							t.Fatalf("failed to copy built stargz blob: %v", err)
						}
						gotData := gotBuf.Bytes()
						got, err := Open(io.NewSectionReader(
							bytes.NewReader(gotBuf.Bytes()), 0, int64(len(gotData))))
						if err != nil {
							t.Fatalf("failed to parse the got stargz: %v", err)
						}

						// Check DiffID is properly calculated
						rc.Close()
						diffID := rc.DiffID()
						wantDiffID := diffIDOfGz(t, gotData)
						if diffID.String() != wantDiffID {
							t.Errorf("DiffID = %q; want %q", diffID, wantDiffID)
						}

						// Compare as stargz
						if !isSameVersion(t, wantData, gotData) {
							t.Errorf("built stargz hasn't same json")
							return
						}
						if !isSameEntries(t, want, got) {
							t.Errorf("built stargz isn't same as the original")
							return
						}

						// Compare as tar.gz
						if !isSameTarGz(t, wantData, gotData) {
							t.Errorf("built stargz isn't same tar.gz")
							return
						}
					})
				}
			}
		}
	}
}

func isSameTarGz(t *testing.T, a, b []byte) bool {
	aGz, err := gzip.NewReader(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("failed to read A as gzip")
	}
	defer aGz.Close()
	bGz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("failed to read B as gzip")
	}
	defer bGz.Close()

	// Same as tar's Next() method but ignores landmarks and TOCJSON file
	next := func(r *tar.Reader) (h *tar.Header, err error) {
		for {
			if h, err = r.Next(); err != nil {
				return
			}
			if h.Name != PrefetchLandmark &&
				h.Name != NoPrefetchLandmark &&
				h.Name != TOCTarName {
				return
			}
		}
	}

	aTar := tar.NewReader(aGz)
	bTar := tar.NewReader(bGz)
	for {
		// Fetch and parse next header.
		aH, aErr := next(aTar)
		bH, bErr := next(bTar)
		if aErr != nil || bErr != nil {
			if aErr == io.EOF && bErr == io.EOF {
				break
			}
			t.Fatalf("Failed to parse tar file: A: %v, B: %v", aErr, bErr)
		}
		if !reflect.DeepEqual(aH, bH) {
			t.Logf("different header (A = %v; B = %v)", aH, bH)
			return false

		}
		aFile, err := ioutil.ReadAll(aTar)
		if err != nil {
			t.Fatal("failed to read tar payload of A")
		}
		bFile, err := ioutil.ReadAll(bTar)
		if err != nil {
			t.Fatal("failed to read tar payload of B")
		}
		if !bytes.Equal(aFile, bFile) {
			t.Logf("different tar payload (A = %q; B = %q)", string(a), string(b))
			return false
		}
	}

	return true
}

func isSameVersion(t *testing.T, a, b []byte) bool {
	ajtoc, _, err := parseStargz(io.NewSectionReader(bytes.NewReader(a), 0, int64(len(a))))
	if err != nil {
		t.Fatalf("failed to parse A: %v", err)
	}
	bjtoc, _, err := parseStargz(io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))))
	if err != nil {
		t.Fatalf("failed to parse B: %v", err)
	}
	t.Logf("A: TOCJSON: %v", dumpTOCJSON(t, ajtoc))
	t.Logf("B: TOCJSON: %v", dumpTOCJSON(t, bjtoc))
	return ajtoc.Version == bjtoc.Version
}

func isSameEntries(t *testing.T, a, b *Reader) bool {
	aroot, ok := a.Lookup("")
	if !ok {
		t.Fatalf("failed to get root of A")
	}
	broot, ok := b.Lookup("")
	if !ok {
		t.Fatalf("failed to get root of B")
	}
	aEntry := stargzEntry{aroot, a}
	bEntry := stargzEntry{broot, b}
	return contains(t, aEntry, bEntry) && contains(t, bEntry, aEntry)
}

type stargzEntry struct {
	e *TOCEntry
	r *Reader
}

// contains checks if all child entries in "b" are also contained in "a".
// This function also checks if the files/chunks contain the same contents among "a" and "b".
func contains(t *testing.T, a, b stargzEntry) bool {
	ae, ar := a.e, a.r
	be, br := b.e, b.r
	t.Logf("Comparing: %q vs %q", ae.Name, be.Name)
	if !equalEntry(ae, be) {
		t.Logf("%q != %q: entry: a: %v, b: %v", ae.Name, be.Name, ae, be)
		return false
	}
	if ae.Type == "dir" {
		t.Logf("Directory: %q vs %q: %v vs %v", ae.Name, be.Name,
			allChildrenName(ae), allChildrenName(be))
		iscontain := true
		ae.ForeachChild(func(aBaseName string, aChild *TOCEntry) bool {
			// Walk through all files on this stargz file.

			if aChild.Name == PrefetchLandmark ||
				aChild.Name == NoPrefetchLandmark {
				return true // Ignore landmarks
			}

			// Ignore a TOCEntry of "./" (formated as "" by stargz lib) on root directory
			// because this points to the root directory itself.
			if aChild.Name == "" && ae.Name == "" {
				return true
			}

			bChild, ok := be.LookupChild(aBaseName)
			if !ok {
				t.Logf("%q (base: %q): not found in b: %v",
					ae.Name, aBaseName, allChildrenName(be))
				iscontain = false
				return false
			}

			childcontain := contains(t, stargzEntry{aChild, a.r}, stargzEntry{bChild, b.r})
			if !childcontain {
				t.Logf("%q != %q: non-equal dir", ae.Name, be.Name)
				iscontain = false
				return false
			}
			return true
		})
		return iscontain
	} else if ae.Type == "reg" {
		af, err := ar.OpenFile(ae.Name)
		if err != nil {
			t.Fatalf("failed to open file %q on A: %v", ae.Name, err)
		}
		bf, err := br.OpenFile(be.Name)
		if err != nil {
			t.Fatalf("failed to open file %q on B: %v", be.Name, err)
		}

		var nr int64
		for nr < ae.Size {
			abytes, anext, aok := readOffset(t, af, nr, a)
			bbytes, bnext, bok := readOffset(t, bf, nr, b)
			if !aok && !bok {
				break
			} else if !(aok && bok) || anext != bnext {
				t.Logf("%q != %q (offset=%d): chunk existence a=%v vs b=%v, anext=%v vs bnext=%v",
					ae.Name, be.Name, nr, aok, bok, anext, bnext)
				return false
			}
			nr = anext
			if !bytes.Equal(abytes, bbytes) {
				t.Logf("%q != %q: different contents %v vs %v",
					ae.Name, be.Name, string(abytes), string(bbytes))
				return false
			}
		}
		return true
	}

	return true
}

func allChildrenName(e *TOCEntry) (children []string) {
	e.ForeachChild(func(baseName string, _ *TOCEntry) bool {
		children = append(children, baseName)
		return true
	})
	return
}

func equalEntry(a, b *TOCEntry) bool {
	// Here, we selectively compare fileds that we are interested in.
	return a.Name == b.Name &&
		a.Type == b.Type &&
		a.Size == b.Size &&
		a.ModTime3339 == b.ModTime3339 &&
		a.Stat().ModTime().Equal(b.Stat().ModTime()) && // modTime     time.Time
		a.LinkName == b.LinkName &&
		a.Mode == b.Mode &&
		a.UID == b.UID &&
		a.GID == b.GID &&
		a.Uname == b.Uname &&
		a.Gname == b.Gname &&
		(a.Offset > 0) == (b.Offset > 0) &&
		(a.NextOffset() > 0) == (b.NextOffset() > 0) &&
		a.DevMajor == b.DevMajor &&
		a.DevMinor == b.DevMinor &&
		a.NumLink == b.NumLink &&
		reflect.DeepEqual(a.Xattrs, b.Xattrs) &&
		// chunk-related infomations aren't compared in this function.
		// ChunkOffset int64 `json:"chunkOffset,omitempty"`
		// ChunkSize   int64 `json:"chunkSize,omitempty"`
		// children map[string]*TOCEntry
		a.Digest == b.Digest
}

func readOffset(t *testing.T, r *io.SectionReader, offset int64, e stargzEntry) ([]byte, int64, bool) {
	ce, ok := e.r.ChunkEntryForOffset(e.e.Name, offset)
	if !ok {
		return nil, 0, false
	}
	data := make([]byte, ce.ChunkSize)
	t.Logf("Offset: %v, NextOffset: %v", ce.Offset, ce.NextOffset())
	n, err := r.ReadAt(data, ce.ChunkOffset)
	if err != nil {
		t.Fatalf("failed to read file payload of %q (offset:%d,size:%d): %v",
			e.e.Name, ce.ChunkOffset, ce.ChunkSize, err)
	}
	if int64(n) != ce.ChunkSize {
		t.Fatalf("unexpected copied data size %d; want %d",
			n, ce.ChunkSize)
	}
	return data[:n], offset + ce.ChunkSize, true
}

func dumpTOCJSON(t *testing.T, tocJSON *jtoc) string {
	jtocData, err := json.Marshal(*tocJSON)
	if err != nil {
		t.Fatalf("failed to marshal TOC JSON: %v", err)
	}
	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, bytes.NewReader(jtocData)); err != nil {
		t.Fatalf("failed to read toc json blob: %v", err)
	}
	return buf.String()
}

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
			srcCompression := srcCompression
			for _, logprefix := range allowedPrefix {
				logprefix := logprefix
				for _, tarprefix := range allowedPrefix {
					tarprefix := tarprefix
					t.Run(fmt.Sprintf("%s-logprefix=%q-tarprefix=%q", tt.name, logprefix, tarprefix), func(t *testing.T) {
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
						rc, err := Build(compressBlob(t, buildTarStatic(t, tt.in, tarprefix), srcCompression),
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
						wantTar := tar.NewReader(buildTarStatic(t, tt.want, tarprefix))
						for {
							// Fetch and parse next header.
							gotH, wantH, err := next(t, gotTar, wantTar)
							if err != nil {
								if err == io.EOF {
									break
								} else {
									t.Fatalf("Failed to parse tar file: %v", err)
								}
							}

							if !reflect.DeepEqual(gotH, wantH) {
								t.Errorf("different header (got = name:%q,type:%d,size:%d; want = name:%q,type:%d,size:%d)",
									gotH.Name, gotH.Typeflag, gotH.Size, wantH.Name, wantH.Typeflag, wantH.Size)
								return

							}

							got, err := ioutil.ReadAll(gotTar)
							if err != nil {
								t.Fatal("failed to read got tar payload")
							}
							want, err := ioutil.ReadAll(wantTar)
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

const chunkSize = 3

type check func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest, compressionLevel int)

// TestDigestAndVerify runs specified checks against sample stargz blobs.
func TestDigestAndVerify(t *testing.T) {
	tests := []struct {
		name    string
		tarInit func(t *testing.T, dgstMap map[string]digest.Digest) (blob []tarEntry)
		checks  []check
	}{
		{
			name: "no-regfile",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob []tarEntry) {
				return tarOf(
					dir("test/"),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(buildTarStatic(t, tarOf(
					dir("test2/"), // modified
				), allowedPrefix[0])),
			},
		},
		{
			name: "small-files",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob []tarEntry) {
				return tarOf(
					regDigest(t, "baz.txt", "", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					dir("test/"),
					regDigest(t, "test/bar.txt", "bbb", dgstMap),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(buildTarStatic(t, tarOf(
					file("baz.txt", ""),
					file("foo.txt", "M"), // modified
					dir("test/"),
					file("test/bar.txt", "bbb"),
				), allowedPrefix[0])),
				checkVerifyInvalidTOCEntryFail("foo.txt"),
				checkVerifyBrokenContentFail("foo.txt"),
			},
		},
		{
			name: "big-files",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob []tarEntry) {
				return tarOf(
					regDigest(t, "baz.txt", "bazbazbazbazbazbazbaz", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					dir("test/"),
					regDigest(t, "test/bar.txt", "testbartestbar", dgstMap),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(buildTarStatic(t, tarOf(
					file("baz.txt", "bazbazbazMMMbazbazbaz"), // modified
					file("foo.txt", "a"),
					dir("test/"),
					file("test/bar.txt", "testbartestbar"),
				), allowedPrefix[0])),
				checkVerifyInvalidTOCEntryFail("test/bar.txt"),
				checkVerifyBrokenContentFail("test/bar.txt"),
			},
		},
		{
			name: "with-non-regfiles",
			tarInit: func(t *testing.T, dgstMap map[string]digest.Digest) (blob []tarEntry) {
				return tarOf(
					regDigest(t, "baz.txt", "bazbazbazbazbazbazbaz", dgstMap),
					regDigest(t, "foo.txt", "a", dgstMap),
					symlink("barlink", "test/bar.txt"),
					dir("test/"),
					regDigest(t, "test/bar.txt", "testbartestbar", dgstMap),
					dir("test2/"),
					link("test2/bazlink", "baz.txt"),
				)
			},
			checks: []check{
				checkStargzTOC,
				checkVerifyTOC,
				checkVerifyInvalidStargzFail(buildTarStatic(t, tarOf(
					file("baz.txt", "bazbazbazbazbazbazbaz"),
					file("foo.txt", "a"),
					symlink("barlink", "test/bar.txt"),
					dir("test/"),
					file("test/bar.txt", "testbartestbar"),
					dir("test2/"),
					link("test2/bazlink", "foo.txt"), // modified
				), allowedPrefix[0])),
				checkVerifyInvalidTOCEntryFail("test/bar.txt"),
				checkVerifyBrokenContentFail("test/bar.txt"),
			},
		},
	}

	for _, tt := range tests {
		for _, cl := range compressionLevels {
			cl := cl
			for _, srcCompression := range srcCompressions {
				srcCompression := srcCompression
				for _, prefix := range allowedPrefix {
					prefix := prefix
					t.Run(tt.name+"-"+fmt.Sprintf("compression=%v-prefix=%q", cl, prefix), func(t *testing.T) {
						// Get original tar file and chunk digests
						dgstMap := make(map[string]digest.Digest)
						tarBlob := buildTarStatic(t, tt.tarInit(t, dgstMap), prefix)

						rc, err := Build(compressBlob(t, tarBlob, srcCompression), WithChunkSize(chunkSize), WithCompressionLevel(cl))
						if err != nil {
							t.Fatalf("failed to convert stargz: %v", err)
						}
						tocDigest := rc.TOCDigest()
						defer rc.Close()
						buf := new(bytes.Buffer)
						if _, err := io.Copy(buf, rc); err != nil {
							t.Fatalf("failed to copy built stargz blob: %v", err)
						}
						newStargz := buf.Bytes()
						// NoPrefetchLandmark is added during `Bulid`, which is expected behaviour.
						dgstMap[chunkID(NoPrefetchLandmark, 0, int64(len([]byte{landmarkContents})))] = digest.FromBytes([]byte{landmarkContents})

						for _, check := range tt.checks {
							check(t, newStargz, tocDigest, dgstMap, cl)
						}
					})
				}
			}
		}
	}
}

// checkStargzTOC checks the TOC JSON of the passed stargz has the expected
// digest and contains valid chunks. It walks all entries in the stargz and
// checks all chunk digests stored to the TOC JSON match the actual contents.
func checkStargzTOC(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest, compressionLevel int) {
	sgz, err := Open(io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))))
	if err != nil {
		t.Errorf("failed to parse converted stargz: %v", err)
		return
	}
	digestMapTOC, err := listDigests(io.NewSectionReader(
		bytes.NewReader(sgzData), 0, int64(len(sgzData))))
	if err != nil {
		t.Fatalf("failed to list digest: %v", err)
	}
	found := make(map[string]bool)
	for id := range dgstMap {
		found[id] = false
	}
	zr, err := gzip.NewReader(bytes.NewReader(sgzData))
	if err != nil {
		t.Fatalf("failed to decompress converted stargz: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				t.Errorf("failed to read tar entry: %v", err)
				return
			}
			break
		}
		if h.Name == TOCTarName {
			// Check the digest of TOC JSON based on the actual contents
			// It's sure that TOC JSON exists in this archive because
			// Open succeeded.
			dgstr := digest.Canonical.Digester()
			if _, err := io.Copy(dgstr.Hash(), tr); err != nil {
				t.Fatalf("failed to calculate digest of TOC JSON: %v",
					err)
			}
			if dgstr.Digest() != tocDigest {
				t.Errorf("invalid TOC JSON %q; want %q", tocDigest, dgstr.Digest())
			}
			continue
		}
		if _, ok := sgz.Lookup(h.Name); !ok {
			t.Errorf("lost stargz entry %q in the converted TOC", h.Name)
			return
		}
		var n int64
		for n < h.Size {
			ce, ok := sgz.ChunkEntryForOffset(h.Name, n)
			if !ok {
				t.Errorf("lost chunk %q(offset=%d) in the converted TOC",
					h.Name, n)
				return
			}

			// Get the original digest to make sure the file contents are kept unchanged
			// from the original tar, during the whole conversion steps.
			id := chunkID(h.Name, n, ce.ChunkSize)
			want, ok := dgstMap[id]
			if !ok {
				t.Errorf("Unexpected chunk %q(offset=%d,size=%d): %v",
					h.Name, n, ce.ChunkSize, dgstMap)
				return
			}
			found[id] = true

			// Check the file contents
			dgstr := digest.Canonical.Digester()
			if _, err := io.CopyN(dgstr.Hash(), tr, ce.ChunkSize); err != nil {
				t.Fatalf("failed to calculate digest of %q (offset=%d,size=%d)",
					h.Name, n, ce.ChunkSize)
			}
			if want != dgstr.Digest() {
				t.Errorf("Invalid contents in converted stargz %q: %q; want %q",
					h.Name, dgstr.Digest(), want)
				return
			}

			// Check the digest stored in TOC JSON
			dgstTOC, ok := digestMapTOC[ce.Offset]
			if !ok {
				t.Errorf("digest of %q(offset=%d,size=%d,chunkOffset=%d) isn't registered",
					h.Name, ce.Offset, ce.ChunkSize, ce.ChunkOffset)
			}
			if want != dgstTOC {
				t.Errorf("Invalid digest in TOCEntry %q: %q; want %q",
					h.Name, dgstTOC, want)
				return
			}

			n += ce.ChunkSize
		}
	}

	for id, ok := range found {
		if !ok {
			t.Errorf("required chunk %q not found in the converted stargz: %v", id, found)
		}
	}
}

// checkVerifyTOC checks the verification works for the TOC JSON of the passed
// stargz. It walks all entries in the stargz and checks the verifications for
// all chunks work.
func checkVerifyTOC(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest, compressionLevel int) {
	sgz, err := Open(io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))))
	if err != nil {
		t.Errorf("failed to parse converted stargz: %v", err)
		return
	}
	ev, err := sgz.VerifyTOC(tocDigest)
	if err != nil {
		t.Errorf("failed to verify stargz: %v", err)
		return
	}

	found := make(map[string]bool)
	for id := range dgstMap {
		found[id] = false
	}
	zr, err := gzip.NewReader(bytes.NewReader(sgzData))
	if err != nil {
		t.Fatalf("failed to decompress converted stargz: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				t.Errorf("failed to read tar entry: %v", err)
				return
			}
			break
		}
		if h.Name == TOCTarName {
			continue
		}
		if _, ok := sgz.Lookup(h.Name); !ok {
			t.Errorf("lost stargz entry %q in the converted TOC", h.Name)
			return
		}
		var n int64
		for n < h.Size {
			ce, ok := sgz.ChunkEntryForOffset(h.Name, n)
			if !ok {
				t.Errorf("lost chunk %q(offset=%d) in the converted TOC",
					h.Name, n)
				return
			}

			v, err := ev.Verifier(ce)
			if err != nil {
				t.Errorf("failed to get verifier for %q(offset=%d)", h.Name, n)
			}

			found[chunkID(h.Name, n, ce.ChunkSize)] = true

			// Check the file contents
			if _, err := io.CopyN(v, tr, ce.ChunkSize); err != nil {
				t.Fatalf("failed to get chunk of %q (offset=%d,size=%d)",
					h.Name, n, ce.ChunkSize)
			}
			if !v.Verified() {
				t.Errorf("Invalid contents in converted stargz %q (should be succeeded)",
					h.Name)
				return
			}
			n += ce.ChunkSize
		}
	}

	for id, ok := range found {
		if !ok {
			t.Errorf("required chunk %q not found in the converted stargz: %v", id, found)
		}
	}
}

// checkVerifyInvalidTOCEntryFail checks if misconfigured TOC JSON can be
// detected during the verification and the verification returns an error.
func checkVerifyInvalidTOCEntryFail(filename string) check {
	return func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest, compressionLevel int) {
		funcs := map[string]rewriteFunc{
			"lost digest in a entry": func(t *testing.T, toc *jtoc, sgz *io.SectionReader) {
				var found bool
				for _, e := range toc.Entries {
					if cleanEntryName(e.Name) == filename {
						if e.Type != "reg" && e.Type != "chunk" {
							t.Fatalf("entry %q to break must be regfile or chunk", filename)
						}
						if e.ChunkDigest == "" {
							t.Fatalf("entry %q is already invalid", filename)
						}
						e.ChunkDigest = ""
						found = true
					}
				}
				if !found {
					t.Fatalf("rewrite target not found")
				}
			},
			"duplicated entry offset": func(t *testing.T, toc *jtoc, sgz *io.SectionReader) {
				var (
					sampleEntry *TOCEntry
					targetEntry *TOCEntry
				)
				for _, e := range toc.Entries {
					if e.Type == "reg" || e.Type == "chunk" {
						if cleanEntryName(e.Name) == filename {
							targetEntry = e
						} else {
							sampleEntry = e
						}
					}
				}
				if sampleEntry == nil {
					t.Fatalf("TOC must contain at least one regfile or chunk entry other than the rewrite target")
				}
				if targetEntry == nil {
					t.Fatalf("rewrite target not found")
				}
				targetEntry.Offset = sampleEntry.Offset
			},
		}

		for name, rFunc := range funcs {
			t.Run(name, func(t *testing.T) {
				newSgz, newTocDigest := rewriteTOCJSON(t, io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))), rFunc, compressionLevel)
				buf := new(bytes.Buffer)
				if _, err := io.Copy(buf, newSgz); err != nil {
					t.Fatalf("failed to get converted stargz")
				}
				isgz := buf.Bytes()

				sgz, err := Open(io.NewSectionReader(bytes.NewReader(isgz), 0, int64(len(isgz))))
				if err != nil {
					t.Fatalf("failed to parse converted stargz: %v", err)
					return
				}
				_, err = sgz.VerifyTOC(newTocDigest)
				if err == nil {
					t.Errorf("must fail for invalid TOC")
					return
				}
			})
		}
	}
}

// checkVerifyInvalidStargzFail checks if the verification detects that the
// given stargz file doesn't match to the expected digest and returns error.
func checkVerifyInvalidStargzFail(invalid *io.SectionReader) check {
	return func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest, compressionLevel int) {
		rc, err := Build(invalid, WithChunkSize(chunkSize), WithCompressionLevel(compressionLevel))
		if err != nil {
			t.Fatalf("failed to convert stargz: %v", err)
		}
		defer rc.Close()
		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, rc); err != nil {
			t.Fatalf("failed to copy built stargz blob: %v", err)
		}
		mStargz := buf.Bytes()

		sgz, err := Open(io.NewSectionReader(bytes.NewReader(mStargz), 0, int64(len(mStargz))))
		if err != nil {
			t.Fatalf("failed to parse converted stargz: %v", err)
			return
		}
		_, err = sgz.VerifyTOC(tocDigest)
		if err == nil {
			t.Errorf("must fail for invalid TOC")
			return
		}
	}
}

// checkVerifyBrokenContentFail checks if the verifier detects broken contents
// that doesn't match to the expected digest and returns error.
func checkVerifyBrokenContentFail(filename string) check {
	return func(t *testing.T, sgzData []byte, tocDigest digest.Digest, dgstMap map[string]digest.Digest, compressionLevel int) {
		// Parse stargz file
		sgz, err := Open(io.NewSectionReader(bytes.NewReader(sgzData), 0, int64(len(sgzData))))
		if err != nil {
			t.Fatalf("failed to parse converted stargz: %v", err)
			return
		}
		ev, err := sgz.VerifyTOC(tocDigest)
		if err != nil {
			t.Fatalf("failed to verify stargz: %v", err)
			return
		}

		// Open the target file
		sr, err := sgz.OpenFile(filename)
		if err != nil {
			t.Fatalf("failed to open file %q", filename)
		}
		ce, ok := sgz.ChunkEntryForOffset(filename, 0)
		if !ok {
			t.Fatalf("lost chunk %q(offset=%d) in the converted TOC", filename, 0)
			return
		}
		if ce.ChunkSize == 0 {
			t.Fatalf("file mustn't be empty")
			return
		}
		data := make([]byte, ce.ChunkSize)
		if _, err := sr.ReadAt(data, ce.ChunkOffset); err != nil {
			t.Errorf("failed to get data of a chunk of %q(offset=%q)",
				filename, ce.ChunkOffset)
		}

		// Check the broken chunk (must fail)
		v, err := ev.Verifier(ce)
		if err != nil {
			t.Fatalf("failed to get verifier for %q", filename)
		}
		broken := append([]byte{^data[0]}, data[1:]...)
		if _, err := io.CopyN(v, bytes.NewReader(broken), ce.ChunkSize); err != nil {
			t.Fatalf("failed to get chunk of %q (offset=%d,size=%d)",
				filename, ce.ChunkOffset, ce.ChunkSize)
		}
		if v.Verified() {
			t.Errorf("verification must fail for broken file chunk %q(org:%q,broken:%q)",
				filename, data, broken)
		}
	}
}

func chunkID(name string, offset, size int64) string {
	return fmt.Sprintf("%s-%d-%d", cleanEntryName(name), offset, size)
}

type rewriteFunc func(t *testing.T, toc *jtoc, sgz *io.SectionReader)

func rewriteTOCJSON(t *testing.T, sgz *io.SectionReader, rewrite rewriteFunc, compressionLevel int) (newSgz io.Reader, tocDigest digest.Digest) {
	decodedJTOC, jtocOffset, err := parseStargz(sgz)
	if err != nil {
		t.Fatalf("failed to extract TOC JSON: %v", err)
	}

	rewrite(t, decodedJTOC, sgz)

	tocJSON, err := json.Marshal(decodedJTOC)
	if err != nil {
		t.Fatalf("failed to marshal TOC JSON: %v", err)
	}
	dgstr := digest.Canonical.Digester()
	if _, err := io.CopyN(dgstr.Hash(), bytes.NewReader(tocJSON), int64(len(tocJSON))); err != nil {
		t.Fatalf("failed to calculate digest of TOC JSON: %v", err)
	}
	pr, pw := io.Pipe()
	go func() {
		zw, err := gzip.NewWriterLevel(pw, compressionLevel)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		zw.Extra = []byte("stargz.toc")
		tw := tar.NewWriter(zw)
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     TOCTarName,
			Size:     int64(len(tocJSON)),
		}); err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := tw.Write(tocJSON); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := zw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	// Reconstruct stargz file with the modified TOC JSON
	if _, err := sgz.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("failed to reset the seek position of stargz: %v", err)
	}
	return io.MultiReader(
		io.LimitReader(sgz, jtocOffset),          // Original stargz (before TOC JSON)
		pr,                                       // Rewritten TOC JSON
		bytes.NewReader(footerBytes(jtocOffset)), // Unmodified footer (because tocOffset is unchanged)
	), dgstr.Digest()
}

func regDigest(t *testing.T, name string, contentStr string, digestMap map[string]digest.Digest) tarEntry {
	if digestMap == nil {
		t.Fatalf("digest map mustn't be nil")
	}
	content := []byte(contentStr)

	var n int64
	for n < int64(len(content)) {
		size := int64(chunkSize)
		remain := int64(len(content)) - n
		if remain < size {
			size = remain
		}
		dgstr := digest.Canonical.Digester()
		if _, err := io.CopyN(dgstr.Hash(), bytes.NewReader(content[n:n+size]), size); err != nil {
			t.Fatalf("failed to calculate digest of %q (name=%q,offset=%d,size=%d)",
				string(content[n:n+size]), name, n, size)
		}
		digestMap[chunkID(name, n, size)] = dgstr.Digest()
		n += size
	}

	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		if err := w.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     prefix + name,
			Size:     int64(len(content)),
		}); err != nil {
			return err
		}
		if _, err := io.CopyN(w, bytes.NewReader(content), int64(len(content))); err != nil {
			return err
		}
		return nil
	})
}

func listDigests(sgz *io.SectionReader) (map[int64]digest.Digest, error) {
	decodedJTOC, _, err := parseStargz(sgz)
	if err != nil {
		return nil, err
	}
	digestMap := make(map[int64]digest.Digest)
	for _, e := range decodedJTOC.Entries {
		if e.Type == "reg" || e.Type == "chunk" {
			if e.Type == "reg" && e.Size == 0 {
				continue // ignores empty file
			}
			if e.ChunkDigest == "" {
				return nil, fmt.Errorf("ChunkDigest of %q(off=%d) not found in TOC JSON",
					e.Name, e.Offset)
			}
			d, err := digest.Parse(e.ChunkDigest)
			if err != nil {
				return nil, err
			}
			digestMap[e.Offset] = d
		}
	}
	return digestMap, nil
}

func longstring(size int) (str string) {
	unit := "long"
	for i := 0; i < size/len(unit)+1; i++ {
		str = fmt.Sprintf("%s%s", str, unit)
	}

	return str[:size]
}

func link(name string, linkname string) tarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeLink,
			Name:       prefix + name,
			Linkname:   linkname,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}

func chardev(name string, major, minor int64) tarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeChar,
			Name:       prefix + name,
			Devmajor:   major,
			Devminor:   minor,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}

func blockdev(name string, major, minor int64) tarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeBlock,
			Name:       prefix + name,
			Devmajor:   major,
			Devminor:   minor,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}
func fifo(name string) tarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeFifo,
			Name:       prefix + name,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}

func prefetchLandmark() tarEntry {
	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		if err := w.WriteHeader(&tar.Header{
			Name:     PrefetchLandmark,
			Typeflag: tar.TypeReg,
			Size:     int64(len([]byte{landmarkContents})),
		}); err != nil {
			return err
		}
		contents := []byte{landmarkContents}
		if _, err := io.CopyN(w, bytes.NewReader(contents), int64(len(contents))); err != nil {
			return err
		}
		return nil
	})
}

func noPrefetchLandmark() tarEntry {
	return tarEntryFunc(func(w *tar.Writer, prefix string) error {
		if err := w.WriteHeader(&tar.Header{
			Name:     NoPrefetchLandmark,
			Typeflag: tar.TypeReg,
			Size:     int64(len([]byte{landmarkContents})),
		}); err != nil {
			return err
		}
		contents := []byte{landmarkContents}
		if _, err := io.CopyN(w, bytes.NewReader(contents), int64(len(contents))); err != nil {
			return err
		}
		return nil
	})
}

func parseStargz(sgz *io.SectionReader) (decodedJTOC *jtoc, jtocOffset int64, err error) {
	// Parse stargz footer and get the offset of TOC JSON
	tocOffset, footerSize, err := OpenFooter(sgz)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "failed to parse footer")
	}

	// Decode the TOC JSON
	tocReader := io.NewSectionReader(sgz, tocOffset, sgz.Size()-tocOffset-footerSize)
	zr, err := gzip.NewReader(tocReader)
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to uncompress TOC JSON targz entry")
	}
	tr := tar.NewReader(zr)
	h, err := tr.Next()
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to get TOC JSON tar entry")
	} else if h.Name != TOCTarName {
		return nil, 0, fmt.Errorf("invalid TOC JSON tar entry name %q; must be %q",
			h.Name, TOCTarName)
	}
	decodedJTOC = new(jtoc)
	if err := json.NewDecoder(tr).Decode(&decodedJTOC); err != nil {
		return nil, 0, errors.Wrap(err, "failed to decode TOC JSON")
	}
	if _, err := tr.Next(); err != io.EOF {
		// We only accept stargz file that its TOC JSON resides at the end of that
		// file to avoid changing the offsets of the following file entries by
		// rewriting TOC JSON (The official stargz lib also puts TOC JSON at the end
		// of the stargz file at this mement).
		// TODO: in the future, we should relax this restriction.
		return nil, 0, errors.New("TOC JSON must reside at the end of targz")
	}

	return decodedJTOC, tocOffset, nil
}

func TestCountReader(t *testing.T) {
	tests := []struct {
		name    string
		ops     func(*countReader) error
		wantPos int64
	}{
		{
			name: "nop",
			ops: func(pw *countReader) error {
				return nil
			},
			wantPos: 0,
		},
		{
			name: "read",
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			ops: func(pw *countReader) error {
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
			pw, err := newCountReader(bytes.NewReader(make([]byte, 100)))
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

func compressBlob(t *testing.T, src *io.SectionReader, srcCompression int) *io.SectionReader {
	buf := new(bytes.Buffer)
	var w io.WriteCloser
	var err error
	if srcCompression == gzipType {
		w = gzip.NewWriter(buf)
	} else if srcCompression == zstdType {
		w, err = zstd.NewWriter(buf)
		if err != nil {
			t.Fatalf("failed to init zstd writer: %v", err)
		}
	} else {
		return src
	}
	src.Seek(0, io.SeekStart)
	if _, err := io.Copy(w, src); err != nil {
		t.Fatalf("failed to compress source")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to finalize compress source")
	}
	data := buf.Bytes()
	return io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))

}
