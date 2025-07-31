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

package testutil

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/metadata"
	tutil "github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
)

var allowedPrefix = [4]string{"", "./", "/", "../"}

var srcCompressions = map[string]tutil.CompressionFactory{
	"zstd-fastest":                        tutil.ZstdCompressionWithLevel(zstd.SpeedFastest),
	"zstd-default":                        tutil.ZstdCompressionWithLevel(zstd.SpeedDefault),
	"zstd-bettercompression":              tutil.ZstdCompressionWithLevel(zstd.SpeedBetterCompression),
	"gzip-no-compression":                 tutil.GzipCompressionWithLevel(gzip.NoCompression),
	"gzip-bestspeed":                      tutil.GzipCompressionWithLevel(gzip.BestSpeed),
	"gzip-bestcompression":                tutil.GzipCompressionWithLevel(gzip.BestCompression),
	"gzip-defaultcompression":             tutil.GzipCompressionWithLevel(gzip.DefaultCompression),
	"gzip-huffmanonly":                    tutil.GzipCompressionWithLevel(gzip.HuffmanOnly),
	"externaltoc-gzip-bestspeed":          tutil.ExternalTOCGzipCompressionWithLevel(gzip.BestSpeed),
	"externaltoc-gzip-bestcompression":    tutil.ExternalTOCGzipCompressionWithLevel(gzip.BestCompression),
	"externaltoc-gzip-defaultcompression": tutil.ExternalTOCGzipCompressionWithLevel(gzip.DefaultCompression),
	"externaltoc-gzip-huffmanonly":        tutil.ExternalTOCGzipCompressionWithLevel(gzip.HuffmanOnly),
}

type ReaderFactory func(sr *io.SectionReader, opts ...metadata.Option) (r TestableReader, err error)

type TestableReader interface {
	metadata.Reader
	NumOfNodes() (i int, _ error)
}

// TestingT is the minimal set of testing.T required to run the
// tests defined in TestReader. This interface exists to prevent
// leaking the testing package from being exposed outside tests.
type TestingT interface {
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Runner allows running subtests of TestingT. This exists instead of adding
// a Run method to TestingT interface because the Run implementation of
// testing.T would not satisfy the interface.
type Runner func(t TestingT, name string, fn func(t TestingT))

type TestRunner struct {
	TestingT
	Runner Runner
}

func (r *TestRunner) Run(name string, run func(*TestRunner)) {
	r.Runner(r.TestingT, name, func(t TestingT) {
		run(&TestRunner{TestingT: t, Runner: r.Runner})
	})
}

// TestReader tests Reader returns correct file metadata.
func TestReader(t *TestRunner, factory ReaderFactory) {
	sampleTime := time.Now().Truncate(time.Second)
	sampleText := "qwer" + "tyui" + "opas" + "dfgh" + "jk"
	randomData, err := tutil.RandomBytes(64000)
	if err != nil {
		t.Fatalf("failed rand.Read: %v", err)
	}
	data64KB := string(randomData)
	tests := []struct {
		name         string
		chunkSize    int
		minChunkSize int
		in           []tutil.TarEntry
		want         []check
	}{
		{
			name: "empty",
			in:   []tutil.TarEntry{},
			want: []check{
				numOfNodes(2), // root dir + prefetch landmark
			},
		},
		{
			name: "files",
			in: []tutil.TarEntry{
				tutil.File("foo", "foofoo", tutil.WithFileMode(0644|os.ModeSetuid)),
				tutil.Dir("bar/"),
				tutil.File("bar/baz.txt", "bazbazbaz", tutil.WithFileOwner(1000, 1000)),
				tutil.File("xxx.txt", "xxxxx", tutil.WithFileModTime(sampleTime)),
				tutil.File("y.txt", "", tutil.WithFileXattrs(map[string]string{"testkey": "testval"})),
			},
			want: []check{
				numOfNodes(7), // root dir + prefetch landmark + 1 dir + 4 files
				hasFile("foo", "foofoo", 6),
				hasMode("foo", 0644|os.ModeSetuid),
				hasFile("bar/baz.txt", "bazbazbaz", 9),
				hasOwner("bar/baz.txt", 1000, 1000),
				hasFile("xxx.txt", "xxxxx", 5),
				hasModTime("xxx.txt", sampleTime),
				hasFile("y.txt", "", 0),
				hasXattrs("y.txt", map[string]string{"testkey": "testval"}),
			},
		},
		{
			name: "dirs",
			in: []tutil.TarEntry{
				tutil.Dir("foo/", tutil.WithDirMode(os.ModeDir|0600|os.ModeSticky)),
				tutil.Dir("foo/bar/", tutil.WithDirOwner(1000, 1000)),
				tutil.File("foo/bar/baz.txt", "testtest"),
				tutil.File("foo/bar/xxxx", "x"),
				tutil.File("foo/bar/yyy", "yyy"),
				tutil.Dir("foo/a/", tutil.WithDirModTime(sampleTime)),
				tutil.Dir("foo/a/1/", tutil.WithDirXattrs(map[string]string{"testkey": "testval"})),
				tutil.File("foo/a/1/2", "1111111111"),
			},
			want: []check{
				numOfNodes(10), // root dir + prefetch landmark + 4 dirs + 4 files
				hasDirChildren("foo", "bar", "a"),
				hasDirChildren("foo/bar", "baz.txt", "xxxx", "yyy"),
				hasDirChildren("foo/a", "1"),
				hasDirChildren("foo/a/1", "2"),
				hasMode("foo", os.ModeDir|0600|os.ModeSticky),
				hasOwner("foo/bar", 1000, 1000),
				hasModTime("foo/a", sampleTime),
				hasXattrs("foo/a/1", map[string]string{"testkey": "testval"}),
				hasFile("foo/bar/baz.txt", "testtest", 8),
				hasFile("foo/bar/xxxx", "x", 1),
				hasFile("foo/bar/yyy", "yyy", 3),
				hasFile("foo/a/1/2", "1111111111", 10),
			},
		},
		{
			name: "hardlinks",
			in: []tutil.TarEntry{
				tutil.File("foo", "foofoo", tutil.WithFileOwner(1000, 1000)),
				tutil.Dir("bar/"),
				tutil.Link("bar/foolink", "foo"),
				tutil.Link("bar/foolink2", "bar/foolink"),
				tutil.Dir("bar/1/"),
				tutil.File("bar/1/baz.txt", "testtest"),
				tutil.Link("barlink", "bar/1/baz.txt"),
				tutil.Symlink("foosym", "bar/foolink2"),
			},
			want: []check{
				numOfNodes(7), // root dir + prefetch landmark + 2 dirs + 1 flie(linked) + 1 file(linked) + 1 symlink
				hasFile("foo", "foofoo", 6),
				hasOwner("foo", 1000, 1000),
				hasFile("bar/foolink", "foofoo", 6),
				hasOwner("bar/foolink", 1000, 1000),
				hasFile("bar/foolink2", "foofoo", 6),
				hasOwner("bar/foolink2", 1000, 1000),
				hasFile("bar/1/baz.txt", "testtest", 8),
				hasFile("barlink", "testtest", 8),
				hasDirChildren("bar", "foolink", "foolink2", "1"),
				hasDirChildren("bar/1", "baz.txt"),
				sameNodes("foo", "bar/foolink", "bar/foolink2"),
				sameNodes("bar/1/baz.txt", "barlink"),
				linkName("foosym", "bar/foolink2"),
				hasNumLink("foo", 3),     // parent dir + 2 links
				hasNumLink("barlink", 2), // parent dir + 1 link
				hasNumLink("bar", 3),     // parent + "." + child's ".."
			},
		},
		{
			name: "various files",
			in: []tutil.TarEntry{
				tutil.Dir("bar/"),
				tutil.File("bar/../bar///////////////////foo", ""),
				tutil.Chardev("bar/cdev", 10, 11),
				tutil.Blockdev("bar/bdev", 100, 101),
				tutil.Fifo("bar/fifo"),
			},
			want: []check{
				numOfNodes(7), // root dir + prefetch landmark + 1 file + 1 dir + 1 cdev + 1 bdev + 1 fifo
				hasFile("bar/foo", "", 0),
				hasChardev("bar/cdev", 10, 11),
				hasBlockdev("bar/bdev", 100, 101),
				hasFifo("bar/fifo"),
			},
		},
		{
			name:      "chunks",
			chunkSize: 4,
			in: []tutil.TarEntry{
				tutil.Dir("foo/"),
				tutil.File("foo/small", sampleText[:2]),
				tutil.File("foo/large", sampleText),
			},
			want: []check{
				numOfNodes(5), // root dir + prefetch landmark + 1 dir + 2 files
				numOfChunks("foo/large", 1+(len(sampleText)/4)),
				hasFileContentsOffset("foo/small", 0, sampleText[:2]),
				hasFileContentsOffset("foo/large", 0, sampleText[0:]),
				hasFileContentsOffset("foo/large", 1, sampleText[1:]),
				hasFileContentsOffset("foo/large", 2, sampleText[2:]),
				hasFileContentsOffset("foo/large", 3, sampleText[3:]),
				hasFileContentsOffset("foo/large", 4, sampleText[4:]),
				hasFileContentsOffset("foo/large", 5, sampleText[5:]),
				hasFileContentsOffset("foo/large", 6, sampleText[6:]),
				hasFileContentsOffset("foo/large", 7, sampleText[7:]),
				hasFileContentsOffset("foo/large", 8, sampleText[8:]),
				hasFileContentsOffset("foo/large", 9, sampleText[9:]),
				hasFileContentsOffset("foo/large", 10, sampleText[10:]),
				hasFileContentsOffset("foo/large", 11, sampleText[11:]),
				hasFileContentsOffset("foo/large", 12, sampleText[12:]),
				hasFileContentsOffset("foo/large", int64(len(sampleText)-1), ""),
			},
		},
		{
			name:         "several_files_in_chunk",
			minChunkSize: 8000,
			in: []tutil.TarEntry{
				tutil.Dir("foo/"),
				tutil.File("foo/foo1", data64KB),
				tutil.File("foo2", "bb"),
				tutil.File("foo22", "ccc"),
				tutil.Dir("bar/"),
				tutil.File("bar/bar.txt", "aaa"),
				tutil.File("foo3", data64KB),
			},
			// NOTE: we assume that the compressed "data64KB" is still larger than 8KB
			// landmark+dir+foo1, foo2+foo22+dir+bar.txt+foo3, TOC, footer
			want: []check{
				numOfNodes(9), // root dir, prefetch landmark, dir, foo1, foo2, foo22, dir, bar.txt, foo3
				hasDirChildren("foo/", "foo1"),
				hasDirChildren("bar/", "bar.txt"),
				hasFile("foo/foo1", data64KB, int64(len(data64KB))),
				hasFile("foo2", "bb", 2),
				hasFile("foo22", "ccc", 3),
				hasFile("bar/bar.txt", "aaa", 3),
				hasFile("foo3", data64KB, int64(len(data64KB))),
				hasFileContentsWithPreRead("foo22", 0, "ccc", chunkInfo{"foo2", "bb", 0, 2, digest.FromString("bb").String()},
					chunkInfo{"bar/bar.txt", "aaa", 0, 3, digest.FromString("aaa").String()}, chunkInfo{"foo3", data64KB, 0, 64000, digest.FromString(data64KB).String()}),
				hasFileContentsOffset("foo/foo1", 0, data64KB),
				hasFileContentsOffset("foo2", 0, "bb"),
				hasFileContentsOffset("foo2", 1, "b"),
				hasFileContentsOffset("foo22", 0, "ccc"),
				hasFileContentsOffset("foo22", 1, "cc"),
				hasFileContentsOffset("foo22", 2, "c"),
				hasFileContentsOffset("bar/bar.txt", 0, "aaa"),
				hasFileContentsOffset("bar/bar.txt", 1, "aa"),
				hasFileContentsOffset("bar/bar.txt", 2, "a"),
				hasFileContentsOffset("foo3", 0, data64KB),
				hasFileContentsOffset("foo3", 1, data64KB[1:]),
				hasFileContentsOffset("foo3", 2, data64KB[2:]),
				hasFileContentsOffset("foo3", int64(len(data64KB)/2), data64KB[len(data64KB)/2:]),
				hasFileContentsOffset("foo3", int64(len(data64KB)-1), data64KB[len(data64KB)-1:]),
			},
		},
		{
			name:         "several_files_in_chunk_chunked",
			minChunkSize: 8000,
			chunkSize:    32000,
			in: []tutil.TarEntry{
				tutil.Dir("foo/"),
				tutil.File("foo/foo1", data64KB),
				tutil.File("foo2", "bb"),
				tutil.Dir("bar/"),
				tutil.File("foo3", data64KB),
			},
			// NOTE: we assume that the compressed chunk of "data64KB" is still larger than 8KB
			// landmark+dir+foo1(1), foo1(2), foo2+dir+foo3(1), foo3(2), TOC, footer
			want: []check{
				numOfNodes(7), // root dir, prefetch landmark, dir, foo1, foo2, dir, foo3
				hasDirChildren("foo", "foo1"),
				hasFile("foo/foo1", data64KB, int64(len(data64KB))),
				hasFile("foo2", "bb", 2),
				hasFile("foo3", data64KB, int64(len(data64KB))),
				hasFileContentsWithPreRead("foo2", 0, "bb", chunkInfo{"foo3", data64KB[:32000], 0, 32000, digest.FromString(data64KB[:32000]).String()}),
				hasFileContentsOffset("foo/foo1", 0, data64KB),
				hasFileContentsOffset("foo/foo1", 1, data64KB[1:]),
				hasFileContentsOffset("foo/foo1", 2, data64KB[2:]),
				hasFileContentsOffset("foo/foo1", int64(len(data64KB)/2), data64KB[len(data64KB)/2:]),
				hasFileContentsOffset("foo/foo1", int64(len(data64KB)-1), data64KB[len(data64KB)-1:]),
				hasFileContentsOffset("foo2", 0, "bb"),
				hasFileContentsOffset("foo2", 1, "b"),
				hasFileContentsOffset("foo3", 0, data64KB),
				hasFileContentsOffset("foo3", 1, data64KB[1:]),
				hasFileContentsOffset("foo3", 2, data64KB[2:]),
				hasFileContentsOffset("foo3", int64(len(data64KB)/2), data64KB[len(data64KB)/2:]),
				hasFileContentsOffset("foo3", int64(len(data64KB)-1), data64KB[len(data64KB)-1:]),
			},
		},
	}
	for _, tt := range tests {
		for _, prefix := range allowedPrefix {
			prefix := prefix
			for srcCompresionName, srcCompression := range srcCompressions {
				srcCompression := srcCompression()

				t.Run(tt.name+"-"+srcCompresionName, func(t *TestRunner) {
					opts := []tutil.BuildEStargzOption{
						tutil.WithBuildTarOptions(tutil.WithPrefix(prefix)),
						tutil.WithEStargzOptions(estargz.WithCompression(srcCompression)),
					}
					if tt.chunkSize > 0 {
						opts = append(opts, tutil.WithEStargzOptions(estargz.WithChunkSize(tt.chunkSize)))
					}
					if tt.minChunkSize > 0 {
						t.Logf("minChunkSize = %d", tt.minChunkSize)
						opts = append(opts, tutil.WithEStargzOptions(estargz.WithMinChunkSize(tt.minChunkSize)))
					}
					esgz, _, err := tutil.BuildEStargz(tt.in, opts...)
					if err != nil {
						t.Fatalf("failed to build sample eStargz: %v", err)
					}

					telemetry, checkCalled := newCalledTelemetry()
					r, err := factory(esgz,
						metadata.WithDecompressors(srcCompression), metadata.WithTelemetry(telemetry))
					if err != nil {
						t.Fatalf("failed to create new reader: %v", err)
					}
					defer r.Close()
					t.Logf("vvvvv Node tree vvvvv")
					t.Logf("[%d] ROOT", r.RootID())
					dumpNodes(t, r, r.RootID(), 1)
					t.Logf("^^^^^^^^^^^^^^^^^^^^^")
					for _, want := range tt.want {
						want(t, r)
					}
					if err := checkCalled(); err != nil {
						t.Errorf("telemetry failure: %v", err)
					}

					// Test the cloned reader works correctly as well
					esgz2, _, err := tutil.BuildEStargz(tt.in, opts...)
					if err != nil {
						t.Fatalf("failed to build sample eStargz: %v", err)
					}
					clonedR, err := r.Clone(esgz2)
					if err != nil {
						t.Fatalf("failed to clone reader: %v", err)
					}
					defer clonedR.Close()
					t.Logf("vvvvv Node tree (cloned) vvvvv")
					t.Logf("[%d] ROOT", clonedR.RootID())
					dumpNodes(t, clonedR.(TestableReader), clonedR.RootID(), 1)
					t.Logf("^^^^^^^^^^^^^^^^^^^^^")
					for _, want := range tt.want {
						want(t, clonedR.(TestableReader))
					}
				})
			}
		}
	}

	t.Run("clone-id-stability", func(t *TestRunner) {
		var mapEntries func(r TestableReader, id uint32, m map[string]uint32) (map[string]uint32, error)
		mapEntries = func(r TestableReader, id uint32, m map[string]uint32) (map[string]uint32, error) {
			if m == nil {
				m = make(map[string]uint32)
			}
			return m, r.ForeachChild(id, func(name string, id uint32, mode os.FileMode) bool {
				m[name] = id
				if _, err := mapEntries(r, id, m); err != nil {
					t.Fatalf("could not map files: %s", err)
					return false
				}
				return true
			})
		}

		in := []tutil.TarEntry{
			tutil.File("foo", "foofoo"),
			tutil.Dir("bar/"),
			tutil.File("bar/zzz.txt", "bazbazbaz"),
			tutil.File("bar/aaa.txt", "bazbazbaz"),
			tutil.File("bar/fff.txt", "bazbazbaz"),
			tutil.File("xxx.txt", "xxxxx"),
			tutil.File("y.txt", ""),
		}

		esgz, _, err := tutil.BuildEStargz(in)
		if err != nil {
			t.Fatalf("failed to build sample eStargz: %v", err)
		}

		r, err := factory(esgz)
		if err != nil {
			t.Fatalf("failed to create new reader: %v", err)
		}

		fileMap, err := mapEntries(r, r.RootID(), nil)
		if err != nil {
			t.Fatalf("could not map files: %s", err)
		}
		cr, err := r.Clone(esgz)
		if err != nil {
			t.Fatalf("could not clone reader: %s", err)
		}
		cloneFileMap, err := mapEntries(cr.(TestableReader), cr.RootID(), nil)
		if err != nil {
			t.Fatalf("could not map files in cloned reader: %s", err)
		}
		if !reflect.DeepEqual(fileMap, cloneFileMap) {
			for f, id := range fileMap {
				t.Logf("original mapping %s -> %d", f, id)
			}
			for f, id := range cloneFileMap {
				t.Logf("clone mapping %s -> %d", f, id)
			}
			t.Fatal("file -> ID mappings did not match between original and cloned reader")
		}
	})
}

func newCalledTelemetry() (telemetry *metadata.Telemetry, check func() error) {
	var getFooterLatencyCalled bool
	var getTocLatencyCalled bool
	var deserializeTocLatencyCalled bool
	return &metadata.Telemetry{
			GetFooterLatency:      func(time.Time) { getFooterLatencyCalled = true },
			GetTocLatency:         func(time.Time) { getTocLatencyCalled = true },
			DeserializeTocLatency: func(time.Time) { deserializeTocLatencyCalled = true },
		}, func() error {
			var errs []error
			if !getFooterLatencyCalled {
				errs = append(errs, fmt.Errorf("metrics GetFooterLatency isn't called"))
			}
			if !getTocLatencyCalled {
				errs = append(errs, fmt.Errorf("metrics GetTocLatency isn't called"))
			}
			if !deserializeTocLatencyCalled {
				errs = append(errs, fmt.Errorf("metrics DeserializeTocLatency isn't called"))
			}
			return errors.Join(errs...)
		}
}

func dumpNodes(t TestingT, r TestableReader, id uint32, level int) {
	if err := r.ForeachChild(id, func(name string, id uint32, mode os.FileMode) bool {
		ind := ""
		for i := 0; i < level; i++ {
			ind += " "
		}
		t.Logf("%v+- [%d] %q : %v", ind, id, name, mode)
		dumpNodes(t, r, id, level+1)
		return true
	}); err != nil {
		t.Errorf("failed to dump nodes %v", err)
	}
}

type check func(TestingT, TestableReader)

func numOfNodes(want int) check {
	return func(t TestingT, r TestableReader) {
		i, err := r.NumOfNodes()
		if err != nil {
			t.Errorf("num of nodes: %v", err)
		}
		if want != i {
			t.Errorf("unexpected num of nodes %d; want %d", i, want)
		}
	}
}

func numOfChunks(name string, num int) check {
	return func(t TestingT, r TestableReader) {
		nr, ok := r.(interface {
			NumOfChunks(id uint32) (i int, _ error)
		})
		if !ok {
			return // skip
		}
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", name, err)
			return
		}
		i, err := nr.NumOfChunks(id)
		if err != nil {
			t.Errorf("failed to get num of chunks of %q: %v", name, err)
			return
		}
		if i != num {
			t.Errorf("unexpected num of chunk of %q : %d want %d", name, i, num)
		}
	}
}

func sameNodes(n string, nodes ...string) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, n)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", n, err)
			return
		}
		for _, en := range nodes {
			eid, err := lookup(r, en)
			if err != nil {
				t.Errorf("failed to lookup %q: %v", en, err)
				return
			}
			if eid != id {
				t.Errorf("unexpected ID of %q: %d want %d", en, eid, id)
			}
		}
	}
}

func linkName(name string, linkName string) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("failed to get attr of %q: %v", name, err)
			return
		}
		if attr.Mode&os.ModeSymlink == 0 {
			t.Errorf("%q is not a symlink: %v", name, attr.Mode)
			return
		}
		if attr.LinkName != linkName {
			t.Errorf("unexpected link name of %q : %q want %q", name, attr.LinkName, linkName)
			return
		}
	}
}

func hasNumLink(name string, numLink int) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("failed to get attr of %q: %v", name, err)
			return
		}
		if attr.NumLink != numLink {
			t.Errorf("unexpected numLink of %q: %d want %d", name, attr.NumLink, numLink)
			return
		}
	}
}

func hasDirChildren(name string, children ...string) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("failed to get attr of %q: %v", name, err)
			return
		}
		if !attr.Mode.IsDir() {
			t.Errorf("%q is not directory: %v", name, attr.Mode)
			return
		}
		found := map[string]struct{}{}
		if err := r.ForeachChild(id, func(name string, id uint32, mode os.FileMode) bool {
			found[name] = struct{}{}
			return true
		}); err != nil {
			t.Errorf("failed to see children %v", err)
			return
		}
		if len(found) != len(children) {
			t.Errorf("unexpected number of children of %q : %d want %d", name, len(found), len(children))
		}
		for _, want := range children {
			if _, ok := found[want]; !ok {
				t.Errorf("expected child %q not found in %q", want, name)
			}
		}
	}
}

func hasChardev(name string, maj, min int) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find chardev %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of chardev %q: %v", name, err)
			return
		}
		if attr.Mode&os.ModeDevice == 0 || attr.Mode&os.ModeCharDevice == 0 {
			t.Errorf("file %q is not a chardev: %v", name, attr.Mode)
			return
		}
		if attr.DevMajor != maj || attr.DevMinor != min {
			t.Errorf("unexpected major/minor of chardev %q: %d/%d want %d/%d", name, attr.DevMajor, attr.DevMinor, maj, min)
			return
		}
	}
}

func hasBlockdev(name string, maj, min int) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find blockdev %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of blockdev %q: %v", name, err)
			return
		}
		if attr.Mode&os.ModeDevice == 0 || attr.Mode&os.ModeCharDevice != 0 {
			t.Errorf("file %q is not a blockdev: %v", name, attr.Mode)
			return
		}
		if attr.DevMajor != maj || attr.DevMinor != min {
			t.Errorf("unexpected major/minor of blockdev %q: %d/%d want %d/%d", name, attr.DevMajor, attr.DevMinor, maj, min)
			return
		}
	}
}

func hasFifo(name string) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find blockdev %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of blockdev %q: %v", name, err)
			return
		}
		if attr.Mode&os.ModeNamedPipe == 0 {
			t.Errorf("file %q is not a fifo: %v", name, attr.Mode)
			return
		}
	}
}

func hasFile(name, content string, size int64) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find file %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of file %q: %v", name, err)
			return
		}
		if !attr.Mode.IsRegular() {
			t.Errorf("file %q is not a regular file: %v", name, attr.Mode)
			return
		}
		sr, err := r.OpenFile(id)
		if err != nil {
			t.Errorf("cannot open file %q: %v", name, err)
			return
		}
		data, err := io.ReadAll(io.NewSectionReader(sr, 0, attr.Size))
		if err != nil {
			t.Errorf("cannot read file %q: %v", name, err)
			return
		}
		if attr.Size != size {
			t.Errorf("unexpected size of file %q : %d (%q) want %d (%q)", name, attr.Size, longBytesView(data), size, longBytesView([]byte(content)))
			return
		}
		if string(data) != content {
			t.Errorf("unexpected content of %q: %q want %q", name, longBytesView(data), longBytesView([]byte(content)))
			return
		}
	}
}

type chunkInfo struct {
	name        string
	data        string
	chunkOffset int64
	chunkSize   int64
	chunkDigest string
}

func hasFileContentsWithPreRead(name string, off int64, contents string, extra ...chunkInfo) check {
	return func(t TestingT, r TestableReader) {
		extraMap := make(map[uint32]chunkInfo)
		for _, e := range extra {
			id, err := lookup(r, e.name)
			if err != nil {
				t.Errorf("failed to lookup extra %q: %v", e.name, err)
				return
			}
			extraMap[id] = e
		}
		var extraNames []string
		for _, e := range extraMap {
			extraNames = append(extraNames, e.name)
		}
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", name, err)
			return
		}
		fr, err := r.OpenFileWithPreReader(id, func(id uint32, chunkOffset, chunkSize int64, chunkDigest string, cr io.Reader) error {
			t.Logf("On %q: got preread of %d", name, id)
			ex, ok := extraMap[id]
			if !ok {
				t.Fatalf("fail on %q: unexpected entry %d: %+v", name, id, extraNames)
			}
			if chunkOffset != ex.chunkOffset || chunkSize != ex.chunkSize || chunkDigest != ex.chunkDigest {
				t.Fatalf("fail on %q: unexpected node %d: %+v", name, id, ex)
			}
			got, err := io.ReadAll(cr)
			if err != nil {
				t.Fatalf("fail on %q: failed to read %d: %v", name, id, err)
			}
			if ex.data != string(got) {
				t.Fatalf("fail on %q: unexpected contents of %d: len=%d; want=%d", name, id, len(got), len(ex.data))
			}
			delete(extraMap, id)
			return nil
		})
		if err != nil {
			t.Errorf("failed to open file %q: %v", name, err)
			return
		}
		buf := make([]byte, len(contents))
		n, err := fr.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			t.Errorf("failed to read file %q (off:%d, want:%q): %v", name, off, contents, err)
			return
		}
		if n != len(contents) {
			t.Errorf("failed to read contents %q (off:%d, want:%q) got %q", name, off, longBytesView([]byte(contents)), longBytesView(buf))
			return
		}
		if string(buf) != contents {
			t.Errorf("unexpected content of %q: %q want %q", name, longBytesView(buf), longBytesView([]byte(contents)))
			return
		}
		if len(extraMap) != 0 {
			var exNames []string
			for _, ex := range extraMap {
				exNames = append(exNames, ex.name)
			}
			t.Fatalf("fail on %q: some entries aren't read: %+v", name, exNames)
		}
	}
}

func hasFileContentsOffset(name string, off int64, contents string) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("failed to lookup %q: %v", name, err)
			return
		}
		fr, err := r.OpenFile(id)
		if err != nil {
			t.Errorf("failed to open file %q: %v", name, err)
			return
		}
		buf := make([]byte, len(contents))
		n, err := fr.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			t.Errorf("failed to read file %q (off:%d, want:%q): %v", name, off, contents, err)
			return
		}
		if n != len(contents) {
			t.Errorf("failed to read contents %q (off:%d, want:%q) got %q", name, off, longBytesView([]byte(contents)), longBytesView(buf))
			return
		}
		if string(buf) != contents {
			t.Errorf("unexpected content of %q: %q want %q", name, longBytesView(buf), longBytesView([]byte(contents)))
			return
		}
	}
}

func hasMode(name string, mode os.FileMode) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find file %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of file %q: %v", name, err)
			return
		}
		if attr.Mode != mode {
			t.Errorf("unexpected mode of %q: %v want %v", name, attr.Mode, mode)
			return
		}
	}
}

func hasOwner(name string, uid, gid int) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find file %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of file %q: %v", name, err)
			return
		}
		if attr.UID != uid || attr.GID != gid {
			t.Errorf("unexpected owner of %q: (%d:%d) want (%d:%d)", name, attr.UID, attr.GID, uid, gid)
			return
		}
	}
}

func hasModTime(name string, modTime time.Time) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find file %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of file %q: %v", name, err)
			return
		}
		attrModTime := attr.ModTime
		if attrModTime.Before(modTime) || attrModTime.After(modTime) {
			t.Errorf("unexpected time of %q: %v; want %v", name, attrModTime, modTime)
			return
		}
	}
}

func hasXattrs(name string, xattrs map[string]string) check {
	return func(t TestingT, r TestableReader) {
		id, err := lookup(r, name)
		if err != nil {
			t.Errorf("cannot find file %q: %v", name, err)
			return
		}
		attr, err := r.GetAttr(id)
		if err != nil {
			t.Errorf("cannot get attr of file %q: %v", name, err)
			return
		}
		if len(attr.Xattrs) != len(xattrs) {
			t.Errorf("unexpected size of xattr of %q: %d want %d", name, len(attr.Xattrs), len(xattrs))
			return
		}
		for k, v := range attr.Xattrs {
			if xattrs[k] != string(v) {
				t.Errorf("unexpected xattr of %q: %q=%q want %q=%q", name, k, string(v), k, xattrs[k])
			}
		}
	}
}

func lookup(r TestableReader, name string) (uint32, error) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		return r.RootID(), nil
	}
	dir, base := filepath.Split(name)
	pid, err := lookup(r, dir)
	if err != nil {
		return 0, err
	}
	id, _, err := r.GetChild(pid, base)
	return id, err
}

// longBytesView is an alias of []byte suitable for printing a long data as an omitted string to avoid long data being printed.
type longBytesView []byte

func (b longBytesView) String() string {
	if len(b) < 100 {
		return string(b)
	}
	return string(b[:50]) + "...(omit)..." + string(b[len(b)-50:])
}
