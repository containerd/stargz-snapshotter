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
	"strings"
	"testing"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz/stargz"
)

// TestBuild tests the resulting stargz blob built by this pkg has the same
// contents as the normal stargz blob.
func TestBuild(t *testing.T) {
	tests := []struct {
		name      string
		chunkSize int
		tarBlob   *io.SectionReader
	}{
		{
			name:      "regfiles and directories",
			chunkSize: 4,
			tarBlob: tarBlob(t,
				reg("foo", "test1"),
				directory("foo2/"),
				reg("foo2/bar", "test2", map[string]string{"test": "sample"}),
			),
		},
		{
			name:      "empty files",
			chunkSize: 4,
			tarBlob: tarBlob(t,
				reg("foo", "tttttt"),
				reg("foo_empty", ""),
				reg("foo2", "tttttt"),
				reg("foo_empty2", ""),
				reg("foo3", "tttttt"),
				reg("foo_empty3", ""),
				reg("foo4", "tttttt"),
				reg("foo_empty4", ""),
				reg("foo5", "tttttt"),
				reg("foo_empty5", ""),
				reg("foo6", "tttttt"),
			),
		},
		{
			name:      "various files",
			chunkSize: 4,
			tarBlob: tarBlob(t,
				reg("baz.txt", "bazbazbazbazbazbazbaz"),
				reg("foo.txt", "a"),
				symlink("barlink", "test/bar.txt"),
				directory("test/"),
				directory("dev/"),
				blockdev("dev/testblock", 3, 4),
				fifo("dev/testfifo"),
				chardev("dev/testchar1", 5, 6),
				reg("test/bar.txt", "testbartestbar", map[string]string{"test2": "sample2"}),
				directory("test2/"),
				link("test2/bazlink", "baz.txt"),
				chardev("dev/testchar2", 1, 2),
			),
		},
		{
			name:      "no contents",
			chunkSize: 4,
			tarBlob: tarBlob(t,
				reg("baz.txt", ""),
				symlink("barlink", "test/bar.txt"),
				directory("test/"),
				directory("dev/"),
				blockdev("dev/testblock", 3, 4),
				fifo("dev/testfifo"),
				chardev("dev/testchar1", 5, 6),
				reg("test/bar.txt", "", map[string]string{"test2": "sample2"}),
				directory("test2/"),
				link("test2/bazlink", "baz.txt"),
				chardev("dev/testchar2", 1, 2),
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			// Test divideEntries()
			entries, err := sort(tt.tarBlob, nil) // identical order
			if err != nil {
				t.Fatalf("faield to parse tar: %v", err)
			}
			var merged []*tarEntry
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
			sw := stargz.NewWriter(wantBuf)
			sw.ChunkSize = tt.chunkSize
			if err := sw.AppendTar(tt.tarBlob); err != nil {
				t.Fatalf("faield to append tar to want stargz: %v", err)
			}
			if err := sw.Close(); err != nil {
				t.Fatalf("faield to prepare want stargz: %v", err)
			}
			wantData := wantBuf.Bytes()
			want, err := stargz.Open(io.NewSectionReader(
				bytes.NewReader(wantData), 0, int64(len(wantData))))
			if err != nil {
				t.Fatalf("failed to parse the want stargz: %v", err)
			}

			// Prepare testing data
			rc, _, err := Build(tt.tarBlob, nil, WithChunkSize(tt.chunkSize))
			if err != nil {
				t.Fatalf("faield to build stargz: %v", err)
			}
			defer rc.Close()
			gotBuf := new(bytes.Buffer)
			if _, err := io.Copy(gotBuf, rc); err != nil {
				t.Fatalf("failed to copy built stargz blob: %v", err)
			}
			gotData := gotBuf.Bytes()
			got, err := stargz.Open(io.NewSectionReader(
				bytes.NewReader(gotBuf.Bytes()), 0, int64(len(gotData))))
			if err != nil {
				t.Fatalf("failed to parse the got stargz: %v", err)
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
				h.Name != stargz.TOCTarName {
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
	ablob, err := parseStargz(io.NewSectionReader(bytes.NewReader(a), 0, int64(len(a))))
	if err != nil {
		t.Fatalf("failed to parse A: %v", err)
	}
	bblob, err := parseStargz(io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))))
	if err != nil {
		t.Fatalf("failed to parse B: %v", err)
	}
	t.Logf("A: TOCJSON: %v", dumpTOCJSON(t, ablob.jtoc))
	t.Logf("B: TOCJSON: %v", dumpTOCJSON(t, bblob.jtoc))
	return ablob.jtoc.Version == bblob.jtoc.Version
}

func isSameEntries(t *testing.T, a, b *stargz.Reader) bool {
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
	e *stargz.TOCEntry
	r *stargz.Reader
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
		ae.ForeachChild(func(aBaseName string, aChild *stargz.TOCEntry) bool {
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
				t.Logf("%q != %q: chunk existence a=%v vs b=%v, anext=%v vs bnext=%v",
					ae.Name, be.Name, aok, bok, anext, bnext)
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

func allChildrenName(e *stargz.TOCEntry) (children []string) {
	e.ForeachChild(func(baseName string, _ *stargz.TOCEntry) bool {
		children = append(children, baseName)
		return true
	})
	return
}

func equalEntry(a, b *stargz.TOCEntry) bool {
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
		name string
		in   *io.SectionReader
		log  []string
		want *io.SectionReader
	}{
		{
			name: "nolog",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				directory("bar/"),
				reg("bar/baz.txt", "baz"),
				reg("bar/bar.txt", "bar"),
			),
			want: tarBlob(t,
				noPrefetchLandmark(),
				reg("foo.txt", "foo"),
				directory("bar/"),
				reg("bar/baz.txt", "baz"),
				reg("bar/bar.txt", "bar"),
			),
		},
		{
			name: "identical",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				directory("bar/"),
				reg("bar/baz.txt", "baz"),
				reg("bar/bar.txt", "bar"),
				reg("bar/baa.txt", "baa"),
			),
			log: []string{"foo.txt", "bar/baz.txt"},
			want: tarBlob(t,
				reg("foo.txt", "foo"),
				directory("bar/"),
				reg("bar/baz.txt", "baz"),
				prefetchLandmark(),
				reg("bar/bar.txt", "bar"),
				reg("bar/baa.txt", "baa"),
			),
		},
		{
			name: "shuffle_reg",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				reg("baz.txt", "baz"),
				reg("bar.txt", "bar"),
				reg("baa.txt", "baa"),
			),
			log: []string{"baa.txt", "bar.txt", "baz.txt"},
			want: tarBlob(t,
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				prefetchLandmark(),
				reg("foo.txt", "foo"),
			),
		},
		{
			name: "shuffle_directory",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				directory("bar/"),
				reg("bar/bar.txt", "bar"),
				reg("bar/baa.txt", "baa"),
				directory("baz/"),
				reg("baz/baz1.txt", "baz"),
				reg("baz/baz2.txt", "baz"),
				directory("baz/bazbaz/"),
				reg("baz/bazbaz/bazbaz_b.txt", "baz"),
				reg("baz/bazbaz/bazbaz_a.txt", "baz"),
			),
			log: []string{"baz/bazbaz/bazbaz_a.txt", "baz/baz2.txt", "foo.txt"},
			want: tarBlob(t,
				directory("baz/"),
				directory("baz/bazbaz/"),
				reg("baz/bazbaz/bazbaz_a.txt", "baz"),
				reg("baz/baz2.txt", "baz"),
				reg("foo.txt", "foo"),
				prefetchLandmark(),
				directory("bar/"),
				reg("bar/bar.txt", "bar"),
				reg("bar/baa.txt", "baa"),
				reg("baz/baz1.txt", "baz"),
				reg("baz/bazbaz/bazbaz_b.txt", "baz"),
			),
		},
		{
			name: "shuffle_link",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				reg("baz.txt", "baz"),
				link("bar.txt", "baz.txt"),
				reg("baa.txt", "baa"),
			),
			log: []string{"baz.txt"},
			want: tarBlob(t,
				reg("baz.txt", "baz"),
				prefetchLandmark(),
				reg("foo.txt", "foo"),
				link("bar.txt", "baz.txt"),
				reg("baa.txt", "baa"),
			),
		},
		{
			name: "longname",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				reg(longname1, "test"),
				directory("bar/"),
				reg("bar/bar.txt", "bar"),
				reg("bar/baa.txt", "baa"),
				reg(fmt.Sprintf("bar/%s", longname2), "test2"),
			),
			log: []string{fmt.Sprintf("bar/%s", longname2), longname1},
			want: tarBlob(t,
				directory("bar/"),
				reg(fmt.Sprintf("bar/%s", longname2), "test2"),
				reg(longname1, "test"),
				prefetchLandmark(),
				reg("foo.txt", "foo"),
				reg("bar/bar.txt", "bar"),
				reg("bar/baa.txt", "baa"),
			),
		},
		{
			name: "various_types",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				symlink("foo2", "foo.txt"),
				chardev("foochar", 10, 50),
				blockdev("fooblock", 15, 20),
				fifo("fifoo"),
			),
			log: []string{"fifoo", "foo2", "foo.txt", "fooblock"},
			want: tarBlob(t,
				fifo("fifoo"),
				symlink("foo2", "foo.txt"),
				reg("foo.txt", "foo"),
				blockdev("fooblock", 15, 20),
				prefetchLandmark(),
				chardev("foochar", 10, 50),
			),
		},
		{
			name: "existing_landmark",
			in: tarBlob(t,
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				prefetchLandmark(),
				reg("foo.txt", "foo"),
			),
			log: []string{"foo.txt", "bar.txt"},
			want: tarBlob(t,
				reg("foo.txt", "foo"),
				reg("bar.txt", "bar"),
				prefetchLandmark(),
				reg("baa.txt", "baa"),
				reg("baz.txt", "baz"),
			),
		},
		{
			name: "existing_landmark_nolog",
			in: tarBlob(t,
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				prefetchLandmark(),
				reg("foo.txt", "foo"),
			),
			want: tarBlob(t,
				noPrefetchLandmark(),
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				reg("foo.txt", "foo"),
			),
		},
		{
			name: "existing_noprefetch_landmark",
			in: tarBlob(t,
				noPrefetchLandmark(),
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				reg("foo.txt", "foo"),
			),
			log: []string{"foo.txt", "bar.txt"},
			want: tarBlob(t,
				reg("foo.txt", "foo"),
				reg("bar.txt", "bar"),
				prefetchLandmark(),
				reg("baa.txt", "baa"),
				reg("baz.txt", "baz"),
			),
		},
		{
			name: "existing_noprefetch_landmark_nolog",
			in: tarBlob(t,
				noPrefetchLandmark(),
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				reg("foo.txt", "foo"),
			),
			want: tarBlob(t,
				noPrefetchLandmark(),
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				reg("baz.txt", "baz"),
				reg("foo.txt", "foo"),
			),
		},
		{
			name: "not_existing_file",
			in: tarBlob(t,
				reg("foo.txt", "foo"),
				reg("baz.txt", "baz"),
				reg("bar.txt", "bar"),
				reg("baa.txt", "baa"),
			),
			log: []string{"baa.txt", "bar.txt", "dummy"},
			want: tarBlob(t,
				reg("baa.txt", "baa"),
				reg("bar.txt", "bar"),
				prefetchLandmark(),
				reg("foo.txt", "foo"),
				reg("baz.txt", "baz"),
			),
		},
		{
			name: "hardlink",
			in: tarBlob(t,
				reg("baz.txt", "aaaaa"),
				link("bazlink", "baz.txt"),
			),
			log: []string{"bazlink"},
			want: tarBlob(t,
				reg("baz.txt", "aaaaa"),
				link("bazlink", "baz.txt"),
				prefetchLandmark(),
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Sort tar file
			entries, err := sort(tt.in, tt.log)
			if err != nil {
				t.Fatalf("failed to sort: %q", err)
			}
			gotTar := tar.NewReader(readerFromEntries(entries...))

			// Compare all
			wantTar := tar.NewReader(tt.want)
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

func next(t *testing.T, a *tar.Reader, b *tar.Reader) (ah *tar.Header, bh *tar.Header, err error) {
	eofA, eofB := false, false

	ah, err = a.Next()
	if err != nil {
		if err == io.EOF {
			eofA = true
		} else {
			t.Fatalf("Failed to parse tar file: %q", err)
		}
	}

	bh, err = b.Next()
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

func longstring(size int) (str string) {
	unit := "long"
	for i := 0; i < size/len(unit)+1; i++ {
		str = fmt.Sprintf("%s%s", str, unit)
	}

	return str[:size]
}

type appender func(w *tar.Writer) error

func tarBlob(t *testing.T, appenders ...appender) *io.SectionReader {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	for _, f := range appenders {
		if err := f(tw); err != nil {
			t.Fatalf("failed to append tar: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Writer.Close() failed: %v", err)
	}
	data := buf.Bytes()
	return io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))
}

func directory(name string) appender {
	if !strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("dir %q must have suffix /", name))
	}
	now := time.Now()
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeDir,
			Name:       name,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	}
}

func reg(name string, contentStr string, opts ...interface{}) appender {
	now := time.Now()
	var xattrs map[string]string
	for _, o := range opts {
		if x, ok := o.(map[string]string); ok {
			xattrs = x
		}
	}
	return func(w *tar.Writer) error {
		if err := w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeReg,
			Name:       name,
			Size:       int64(len(contentStr)),
			Xattrs:     xattrs,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		}); err != nil {
			return err
		}
		if _, err := io.CopyN(w, bytes.NewReader([]byte(contentStr)), int64(len(contentStr))); err != nil {
			return err
		}
		return nil
	}
}

func link(name string, linkname string) appender {
	now := time.Now()
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeLink,
			Name:       name,
			Linkname:   linkname,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	}
}

func symlink(name string, linkname string) appender {
	now := time.Now()
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeSymlink,
			Name:       name,
			Linkname:   linkname,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	}
}

func chardev(name string, major, minor int64) appender {
	now := time.Now()
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeChar,
			Name:       name,
			Devmajor:   major,
			Devminor:   minor,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	}
}

func blockdev(name string, major, minor int64) appender {
	now := time.Now()
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeBlock,
			Name:       name,
			Devmajor:   major,
			Devminor:   minor,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	}
}
func fifo(name string) appender {
	now := time.Now()
	return func(w *tar.Writer) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeFifo,
			Name:       name,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	}
}

func prefetchLandmark() appender {
	return func(w *tar.Writer) error {
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
	}
}

func noPrefetchLandmark() appender {
	return func(w *tar.Writer) error {
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
	}
}
