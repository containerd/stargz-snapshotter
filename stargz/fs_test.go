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

package stargz

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/stargz/reader"
	"github.com/containerd/stargz-snapshotter/stargz/remote"
	"github.com/containerd/stargz-snapshotter/task"
	"github.com/google/crfs/stargz"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

const (
	sampleChunkSize    = 3
	sampleMiddleOffset = sampleChunkSize / 2
	sampleData1        = "0123456789"
	sampleData2        = "abcdefghij"
	lastChunkOffset1   = sampleChunkSize * (int64(len(sampleData1)) / sampleChunkSize)
)

func TestCheck(t *testing.T) {
	bb := &breakBlob{}
	fs := &filesystem{
		layer: map[string]*layer{
			"test": newLayer(bb, nopreader{}, nil, time.Second),
		},
		backgroundTaskManager: task.NewBackgroundTaskManager(1, time.Millisecond),
	}
	bb.success = true
	if err := fs.Check(context.TODO(), "test"); err != nil {
		t.Errorf("connection failed; wanted to succeed")
	}

	bb.success = false
	if err := fs.Check(context.TODO(), "test"); err == nil {
		t.Errorf("connection succeeded; wanted to fail")
	}
}

type nopreader struct{}

func (r nopreader) OpenFile(name string) (io.ReaderAt, error)                     { return nil, nil }
func (r nopreader) Lookup(name string) (*stargz.TOCEntry, bool)                   { return nil, false }
func (r nopreader) CacheTarGzWithReader(ir io.Reader, opts ...cache.Option) error { return nil }

type breakBlob struct {
	success bool
}

func (r *breakBlob) Authn(tr http.RoundTripper) (http.RoundTripper, error)         { return nil, nil }
func (r *breakBlob) Size() int64                                                   { return 10 }
func (r *breakBlob) FetchedSize() int64                                            { return 5 }
func (r *breakBlob) ReadAt(p []byte, o int64, opts ...remote.Option) (int, error)  { return 0, nil }
func (r *breakBlob) Cache(offset int64, size int64, option ...remote.Option) error { return nil }
func (r *breakBlob) Check() error {
	if !r.success {
		return fmt.Errorf("failed")
	}
	return nil
}

// Tests Read method of each file node.
func TestNodeRead(t *testing.T) {
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
	for sn, size := range sizeCond {
		for in, innero := range innerOffsetCond {
			for bo, baseo := range baseOffsetCond {
				for fn, filesize := range fileSizeCond {
					t.Run(fmt.Sprintf("reading_%s_%s_%s_%s", sn, in, bo, fn), func(t *testing.T) {
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

						// data we get from the file node.
						f := makeNodeReader(t, []byte(sampleData1)[:filesize], sampleChunkSize)
						tmpbuf := make([]byte, size) // fuse library can request bigger than remain
						rr, errno := f.Read(context.Background(), tmpbuf, offset)
						if errno != 0 {
							t.Errorf("failed to read off=%d, size=%d, filesize=%d: %v", offset, size, filesize, err)
							return
						}
						if rsize := rr.Size(); int64(rsize) != wantN {
							t.Errorf("read size: %d; want: %d", rsize, wantN)
							return
						}
						tmpbuf = make([]byte, len(tmpbuf))
						respData, fs := rr.Bytes(tmpbuf)
						if fs != fuse.OK {
							t.Errorf("failed to read result data for off=%d, size=%d, filesize=%d: %v", offset, size, filesize, err)
						}

						if !bytes.Equal(wantData, respData) {
							t.Errorf("off=%d, filesize=%d; read data{size=%d,data=%q}; want (size=%d,data=%q)",
								offset, filesize, len(respData), string(respData), wantN, string(wantData))
							return
						}
					})
				}
			}
		}
	}
}

func makeNodeReader(t *testing.T, contents []byte, chunkSize int64) *file {
	testName := "test"
	r, err := stargz.Open(buildStargz(t, []tarent{
		regfile(testName, string(contents)),
	}, chunkSizeInfo(chunkSize)))
	if err != nil {
		t.Fatal("failed to make stargz")
	}
	rootNode := getRootNode(t, r)
	var eo fuse.EntryOut
	inode, errno := rootNode.Lookup(context.Background(), testName, &eo)
	if errno != 0 {
		t.Fatalf("failed to lookup test node; errno: %v", errno)
	}
	f, _, errno := inode.Operations().(fusefs.NodeOpener).Open(context.Background(), 0)
	if errno != 0 {
		t.Fatalf("failed to open test file; errno: %v", errno)
	}
	return f.(*file)
}

func TestExistence(t *testing.T) {
	tests := []struct {
		name string
		in   []tarent
		want []check
	}{
		{
			name: "1_whiteout_with_sibling",
			in: []tarent{
				directory("foo/"),
				regfile("foo/bar.txt", ""),
				regfile("foo/.wh.foo.txt", ""),
			},
			want: []check{
				hasValidWhiteout("foo/foo.txt"),
				fileNotExist("foo/.wh.foo.txt"),
			},
		},
		{
			name: "1_whiteout_with_duplicated_name",
			in: []tarent{
				directory("foo/"),
				regfile("foo/bar.txt", "test"),
				regfile("foo/.wh.bar.txt", ""),
			},
			want: []check{
				hasFileDigest("foo/bar.txt", digestFor("test")),
				fileNotExist("foo/.wh.bar.txt"),
			},
		},
		{
			name: "1_opaque",
			in: []tarent{
				directory("foo/"),
				regfile("foo/.wh..wh..opq", ""),
			},
			want: []check{
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				fileNotExist("foo/.wh..wh..opq"),
			},
		},
		{
			name: "1_opaque_with_sibling",
			in: []tarent{
				directory("foo/"),
				regfile("foo/.wh..wh..opq", ""),
				regfile("foo/bar.txt", "test"),
			},
			want: []check{
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				hasFileDigest("foo/bar.txt", digestFor("test")),
				fileNotExist("foo/.wh..wh..opq"),
			},
		},
		{
			name: "1_opaque_with_xattr",
			in: []tarent{
				directory("foo/", xAttr{"foo": "bar"}),
				regfile("foo/.wh..wh..opq", ""),
			},
			want: []check{
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				hasNodeXattrs("foo/", "foo", "bar"),
				fileNotExist("foo/.wh..wh..opq"),
			},
		},
		{
			name: "prefetch_landmark",
			in: []tarent{
				regfile(PrefetchLandmark, "test"),
				directory("foo/"),
				regfile(fmt.Sprintf("foo/%s", PrefetchLandmark), "test"),
			},
			want: []check{
				fileNotExist(PrefetchLandmark),
				hasFileDigest(fmt.Sprintf("foo/%s", PrefetchLandmark), digestFor("test")),
			},
		},
		{
			name: "no_prefetch_landmark",
			in: []tarent{
				regfile(NoPrefetchLandmark, "test"),
				directory("foo/"),
				regfile(fmt.Sprintf("foo/%s", NoPrefetchLandmark), "test"),
			},
			want: []check{
				fileNotExist(NoPrefetchLandmark),
				hasFileDigest(fmt.Sprintf("foo/%s", NoPrefetchLandmark), digestFor("test")),
			},
		},
		{
			name: "state_file",
			in: []tarent{
				regfile("test", "test"),
			},
			want: []check{
				hasFileDigest("test", digestFor("test")),
				hasStateFile(t, testStateLayerDigest+".json"),
			},
		},
		{
			name: "file_suid",
			in: []tarent{
				regfile("test", "test", os.ModeSetuid),
			},
			want: []check{
				hasExtraMode("test", os.ModeSetuid),
			},
		},
		{
			name: "dir_sgid",
			in: []tarent{
				directory("test/", os.ModeSetgid),
			},
			want: []check{
				hasExtraMode("test/", os.ModeSetgid),
			},
		},
		{
			name: "file_sticky",
			in: []tarent{
				regfile("test", "test", os.ModeSticky),
			},
			want: []check{
				hasExtraMode("test", os.ModeSticky),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := stargz.Open(buildStargz(t, tt.in))
			if err != nil {
				t.Fatalf("stargz.Open: %v", err)
			}
			rootNode := getRootNode(t, r)
			for _, want := range tt.want {
				want(t, rootNode)
			}
		})
	}
}

const testStateLayerDigest = "sha256:deadbeef"

func getRootNode(t *testing.T, r *stargz.Reader) *node {
	root, ok := r.Lookup("")
	if !ok {
		t.Fatalf("failed to find root in stargz")
	}
	rootNode := &node{
		layer: &testLayer{r},
		e:     root,
		s:     newState(testStateLayerDigest, &dummyBlob{}),
	}
	fusefs.NewNodeFS(rootNode, &fusefs.Options{})
	return rootNode
}

type testLayer struct {
	r *stargz.Reader
}

func (tl *testLayer) OpenFile(name string) (io.ReaderAt, error) {
	return tl.r.OpenFile(name)
}

type dummyBlob struct{}

func (db *dummyBlob) Authn(tr http.RoundTripper) (http.RoundTripper, error)             { return nil, nil }
func (db *dummyBlob) ReadAt(p []byte, offset int64, opts ...remote.Option) (int, error) { return 0, nil }
func (db *dummyBlob) Size() int64                                                       { return 10 }
func (db *dummyBlob) FetchedSize() int64                                                { return 5 }
func (db *dummyBlob) Check() error                                                      { return nil }
func (db *dummyBlob) Cache(offset int64, size int64, option ...remote.Option) error     { return nil }

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

type tarent struct {
	header   *tar.Header
	contents []byte
}

// suid, guid, sticky bits for archive/tar
// https://github.com/golang/go/blob/release-branch.go1.13/src/archive/tar/common.go#L607-L609
const (
	cISUID = 04000 // Set uid
	cISGID = 02000 // Set gid
	cISVTX = 01000 // Save text (sticky bit)
)

func extraModeToTarMode(fm os.FileMode) (tm int64) {
	if fm&os.ModeSetuid != 0 {
		tm |= cISUID
	}
	if fm&os.ModeSetgid != 0 {
		tm |= cISGID
	}
	if fm&os.ModeSticky != 0 {
		tm |= cISVTX
	}
	return
}

func regfile(name string, contents string, opts ...interface{}) tarent {
	if strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("file %q has suffix /", name))
	}
	var xmodes os.FileMode
	for _, opt := range opts {
		if v, ok := opt.(os.FileMode); ok {
			xmodes = v
		}
	}
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0644 | extraModeToTarMode(xmodes),
			Size:     int64(len(contents)),
		},
		contents: []byte(contents),
	}
}

type xAttr map[string]string

func directory(name string, opts ...interface{}) tarent {
	if !strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("dir %q hasn't suffix /", name))
	}
	var (
		xattrs xAttr
		xmodes os.FileMode
	)
	for _, opt := range opts {
		if v, ok := opt.(xAttr); ok {
			xattrs = v
		} else if v, ok := opt.(os.FileMode); ok {
			xmodes = v
		}
	}
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name,
			Mode:     0755 | extraModeToTarMode(xmodes),
			Xattrs:   xattrs,
		},
	}
}

type check func(*testing.T, *node)

func fileNotExist(file string) check {
	return func(t *testing.T, root *node) {
		if _, _, err := getDirentAndNode(t, root, file); err == nil {
			t.Errorf("Node %q exists", file)
		}
	}
}

func hasFileDigest(file string, digest string) check {
	return func(t *testing.T, root *node) {
		_, n, err := getDirentAndNode(t, root, file)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", file, err)
		}
		if ndgst := n.Operations().(*node).e.Digest; ndgst != digest {
			t.Fatalf("Digest(%q) = %q, want %q", file, ndgst, digest)
		}
	}
}

func hasExtraMode(name string, mode os.FileMode) check {
	return func(t *testing.T, root *node) {
		_, n, err := getDirentAndNode(t, root, name)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", name, err)
		}
		var ao fuse.AttrOut
		if errno := n.Operations().(fusefs.NodeGetattrer).Getattr(context.Background(), nil, &ao); errno != 0 {
			t.Fatalf("failed to get attributes of node %q: %v", name, errno)
		}
		a := ao.Attr
		gotMode := a.Mode & (syscall.S_ISUID | syscall.S_ISGID | syscall.S_ISVTX)
		wantMode := extraModeToTarMode(mode)
		if gotMode != uint32(wantMode) {
			t.Fatalf("got mode = %b, want %b", gotMode, wantMode)
		}
	}
}

func hasValidWhiteout(name string) check {
	return func(t *testing.T, root *node) {
		ent, n, err := getDirentAndNode(t, root, name)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", name, err)
		}
		var ao fuse.AttrOut
		if errno := n.Operations().(fusefs.NodeGetattrer).Getattr(context.Background(), nil, &ao); errno != 0 {
			t.Fatalf("failed to get attributes of file %q: %v", name, errno)
		}
		a := ao.Attr
		if a.Ino != ent.Ino {
			t.Errorf("inconsistent inodes %d(Node) != %d(Dirent)", a.Ino, ent.Ino)
			return
		}

		// validate the direntry
		if ent.Mode != syscall.S_IFCHR {
			t.Errorf("whiteout entry %q isn't a char device", name)
			return
		}

		// validate the node
		if a.Mode != syscall.S_IFCHR {
			t.Errorf("whiteout %q has an invalid mode %o; want %o",
				name, a.Mode, syscall.S_IFCHR)
			return
		}
		if a.Rdev != uint32(unix.Mkdev(0, 0)) {
			t.Errorf("whiteout %q has invalid device numbers (%d, %d); want (0, 0)",
				name, unix.Major(uint64(a.Rdev)), unix.Minor(uint64(a.Rdev)))
			return
		}
	}
}

func hasNodeXattrs(entry, name, value string) check {
	return func(t *testing.T, root *node) {
		_, n, err := getDirentAndNode(t, root, entry)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", entry, err)
		}

		// check xattr exists in the xattrs list.
		buf := make([]byte, 1000)
		nb, errno := n.Operations().(fusefs.NodeListxattrer).Listxattr(context.Background(), buf)
		if errno != 0 {
			t.Fatalf("failed to get xattrs list of node %q: %v", entry, err)
		}
		attrs := strings.Split(string(buf[:nb]), "\x00")
		var found bool
		for _, x := range attrs {
			if x == name {
				found = true
			}
		}
		if !found {
			t.Errorf("node %q doesn't have an opaque xattr %q", entry, value)
			return
		}

		// check the xattr has valid value.
		v := make([]byte, len(value))
		nv, errno := n.Operations().(fusefs.NodeGetxattrer).Getxattr(context.Background(), name, v)
		if errno != 0 {
			t.Fatalf("failed to get xattr %q of node %q: %v", name, entry, err)
		}
		if int(nv) != len(value) {
			t.Fatalf("invalid xattr size for file %q, value %q got %d; want %d",
				name, value, nv, len(value))
		}
		if string(v) != value {
			t.Errorf("node %q has an invalid xattr %q; want %q", entry, v, value)
			return
		}
	}
}

func hasEntry(t *testing.T, name string, ents fusefs.DirStream) (fuse.DirEntry, bool) {
	for ents.HasNext() {
		de, errno := ents.Next()
		if errno != 0 {
			t.Fatalf("faield to read entries for %q", name)
		}
		if de.Name == name {
			return de, true
		}
	}
	return fuse.DirEntry{}, false
}

func hasStateFile(t *testing.T, id string) check {
	return func(t *testing.T, root *node) {

		// Check the state dir is hidden on OpenDir for "/"
		ents, errno := root.Readdir(context.Background())
		if errno != 0 {
			t.Errorf("failed to open root directory: %v", errno)
			return
		}
		if _, ok := hasEntry(t, stateDirName, ents); ok {
			t.Errorf("state direntry %q should not be listed", stateDirName)
			return
		}

		// Check existence of state dir
		var eo fuse.EntryOut
		sti, errno := root.Lookup(context.Background(), stateDirName, &eo)
		if errno != 0 {
			t.Errorf("failed to lookup directory %q: %v", stateDirName, errno)
			return
		}
		st, ok := sti.Operations().(*state)
		if !ok {
			t.Errorf("directory %q isn't a state node", stateDirName)
			return
		}

		// Check existence of state file
		ents, errno = st.Readdir(context.Background())
		if errno != 0 {
			t.Errorf("failed to open directory %q: %v", stateDirName, errno)
			return
		}
		if _, ok := hasEntry(t, id, ents); !ok {
			t.Errorf("direntry %q not found in %q", id, stateDirName)
			return
		}
		inode, errno := st.Lookup(context.Background(), id, &eo)
		if errno != 0 {
			t.Errorf("failed to lookup node %q in %q: %v", id, stateDirName, errno)
			return
		}
		n, ok := inode.Operations().(*statFile)
		if !ok {
			t.Errorf("entry %q isn't a normal node", id)
			return
		}

		// wanted data
		rand.Seed(time.Now().UnixNano())
		wantErr := fmt.Errorf("test-%d", rand.Int63())

		// report the data
		root.s.report(wantErr)

		// obtain file size (check later)
		var ao fuse.AttrOut
		errno = n.Operations().(fusefs.NodeGetattrer).Getattr(context.Background(), nil, &ao)
		if errno != 0 {
			t.Errorf("failed to get attr of state file: %v", errno)
			return
		}
		attr := ao.Attr

		// get data via state file
		tmp := make([]byte, 4096)
		res, errno := n.Read(context.Background(), nil, tmp, 0)
		if errno != 0 {
			t.Errorf("failed to read state file: %v", errno)
			return
		}
		gotState, status := res.Bytes(nil)
		if status != fuse.OK {
			t.Errorf("failed to get result bytes of state file: %v", errno)
			return
		}
		if attr.Size != uint64(len(string(gotState))) {
			t.Errorf("size %d; want %d", attr.Size, len(string(gotState)))
			return
		}

		var j statJSON
		if err := json.Unmarshal(gotState, &j); err != nil {
			t.Errorf("failed to unmarshal %q: %v", string(gotState), err)
			return
		}
		if wantErr.Error() != j.Error {
			t.Errorf("expected error %q, got %q", wantErr.Error(), j.Error)
			return
		}
	}
}

// getDirentAndNode gets dirent and node at the specified path at once and makes
// sure that the both of them exist.
func getDirentAndNode(t *testing.T, root *node, path string) (ent fuse.DirEntry, n *fusefs.Inode, err error) {
	dir, base := filepath.Split(filepath.Clean(path))

	// get the target's parent directory.
	var eo fuse.EntryOut
	d := root
	for _, name := range strings.Split(dir, "/") {
		if len(name) == 0 {
			continue
		}
		di, errno := d.Lookup(context.Background(), name, &eo)
		if errno != 0 {
			err = fmt.Errorf("failed to lookup directory %q: %v", name, errno)
			return
		}
		var ok bool
		if d, ok = di.Operations().(*node); !ok {
			err = fmt.Errorf("directory %q isn't a normal node", name)
			return
		}

	}

	// get the target's direntry.
	ents, errno := d.Readdir(context.Background())
	if errno != 0 {
		err = fmt.Errorf("failed to open directory %q: %v", path, errno)
	}
	ent, ok := hasEntry(t, base, ents)
	if !ok {
		err = fmt.Errorf("direntry %q not found in the parent directory of %q", base, path)
	}

	// get the target's node.
	n, errno = d.Lookup(context.Background(), base, &eo)
	if errno != 0 {
		err = fmt.Errorf("failed to lookup node %q: %v", path, errno)
	}

	return
}

func digestFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum)
}

func TestLazyTransport(t *testing.T) {
	ta := lazyTransport(func() (http.RoundTripper, error) {
		return &okRoundTripper{}, nil
	})

	// Initialize transport
	tr1, err := ta()
	if err != nil {
		t.Fatalf("failed to initialize transport: %v", err)
	}
	if tr1 == nil {
		t.Errorf("initialized transport is nil")
		return
	}

	// Get the created transport again
	tr2, err := ta()
	if err != nil {
		t.Fatalf("failed to get transport: %v", err)
	}
	if tr2 == nil {
		t.Errorf("passed transport is nil")
		return
	}

	// Check if these transports are same
	if tr1 != tr2 {
		t.Errorf("lazyTransport gave different transports on each time")
		return
	}
}

type okRoundTripper struct{}

func (tr *okRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
	}, nil
}

// Tests prefetch method of each stargz file.
func TestPrefetch(t *testing.T) {
	prefetchLandmarkFile := regfile(PrefetchLandmark, "test")
	noPrefetchLandmarkFile := regfile(NoPrefetchLandmark, "test")
	defaultPrefetchSize := int64(10000)
	landmarkPosition := func(l *layer) int64 {
		if e, ok := l.reader.Lookup(PrefetchLandmark); ok {
			return e.Offset
		}
		return defaultPrefetchSize
	}
	defaultPrefetchPosition := func(l *layer) int64 {
		return l.blob.Size()
	}
	tests := []struct {
		name         string
		in           []tarent
		wantNum      int      // number of chunks wanted in the cache
		wants        []string // filenames to compare
		prefetchSize func(*layer) int64
	}{
		{
			name: "default_prefetch",
			in: []tarent{
				regfile("foo.txt", sampleData1),
			},
			wantNum:      chunkNum(sampleData1),
			wants:        []string{"foo.txt"},
			prefetchSize: defaultPrefetchPosition,
		},
		{
			name: "no_prefetch",
			in: []tarent{
				noPrefetchLandmarkFile,
				regfile("foo.txt", sampleData1),
			},
			wantNum: 0,
		},
		{
			name: "prefetch",
			in: []tarent{
				regfile("foo.txt", sampleData1),
				prefetchLandmarkFile,
				regfile("bar.txt", sampleData2),
			},
			wantNum:      chunkNum(sampleData1),
			wants:        []string{"foo.txt"},
			prefetchSize: landmarkPosition,
		},
		{
			name: "with_dir",
			in: []tarent{
				directory("foo/"),
				regfile("foo/bar.txt", sampleData1),
				prefetchLandmarkFile,
				directory("buz/"),
				regfile("buz/buzbuz.txt", sampleData2),
			},
			wantNum:      chunkNum(sampleData1),
			wants:        []string{"foo/bar.txt"},
			prefetchSize: landmarkPosition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := buildStargz(t, tt.in, chunkSizeInfo(sampleChunkSize))
			blob := newBlob(sr)
			cache := &testCache{membuf: map[string]string{}, t: t}
			gr, _, err := reader.NewReader(sr, cache)
			if err != nil {
				t.Fatalf("failed to make stargz reader: %v", err)
			}
			l := newLayer(blob, gr, nil, time.Second)
			prefetchSize := int64(0)
			if tt.prefetchSize != nil {
				prefetchSize = tt.prefetchSize(l)
			}
			if err := l.prefetch(defaultPrefetchSize); err != nil {
				t.Errorf("failed to prefetch: %v", err)
				return
			}
			if blob.calledPrefetchOffset != 0 {
				t.Errorf("invalid prefetch offset %d; want %d",
					blob.calledPrefetchOffset, 0)
			}
			if blob.calledPrefetchSize != prefetchSize {
				t.Errorf("invalid prefetch size %d; want %d",
					blob.calledPrefetchSize, prefetchSize)
			}
			if tt.wantNum != len(cache.membuf) {
				t.Errorf("number of chunks in the cache %d; want %d: %v", len(cache.membuf), tt.wantNum, err)
				return
			}

			for _, file := range tt.wants {
				e, ok := gr.Lookup(file)
				if !ok {
					t.Fatalf("failed to lookup %q", file)
				}
				wantFile, err := gr.OpenFile(file)
				if err != nil {
					t.Fatalf("failed to open file %q", file)
				}
				blob.readCalled = false
				if _, err := io.Copy(ioutil.Discard, io.NewSectionReader(wantFile, 0, e.Size)); err != nil {
					t.Fatalf("failed to read file %q", file)
				}
				if blob.readCalled {
					t.Errorf("chunks of file %q aren't cached", file)
					return
				}
			}
		})
	}
}

func chunkNum(data string) int {
	return (len(data)-1)/sampleChunkSize + 1
}

func newBlob(sr *io.SectionReader) *sampleBlob {
	return &sampleBlob{
		r: sr,
	}
}

type sampleBlob struct {
	r                    *io.SectionReader
	readCalled           bool
	calledPrefetchOffset int64
	calledPrefetchSize   int64
}

func (sb *sampleBlob) Authn(tr http.RoundTripper) (http.RoundTripper, error) { return nil, nil }
func (sb *sampleBlob) Check() error                                          { return nil }
func (sb *sampleBlob) Size() int64                                           { return sb.r.Size() }
func (sb *sampleBlob) FetchedSize() int64                                    { return 0 }
func (sb *sampleBlob) ReadAt(p []byte, offset int64, opts ...remote.Option) (int, error) {
	sb.readCalled = true
	return sb.r.ReadAt(p, offset)
}
func (sb *sampleBlob) Cache(offset int64, size int64, option ...remote.Option) error {
	sb.calledPrefetchOffset = offset
	sb.calledPrefetchSize = size
	return nil
}

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

func TestWaiter(t *testing.T) {
	var (
		w         = newWaiter()
		waitTime  = time.Second
		startTime = time.Now()
		doneTime  time.Time
		done      = make(chan struct{})
	)

	w.start()

	go func() {
		defer close(done)
		if err := w.wait(10 * time.Second); err != nil {
			t.Errorf("failed to wait: %v", err)
			return
		}
		doneTime = time.Now()
	}()

	time.Sleep(waitTime)
	w.done()
	<-done

	if doneTime.Sub(startTime) < waitTime {
		t.Errorf("wait time is too short: %v; want %v", doneTime.Sub(startTime), waitTime)
	}
}
