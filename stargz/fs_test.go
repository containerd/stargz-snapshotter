/*
   Copyright The containerd Authors.
   Copyright 2019 The Go Authors.

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
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/crfs/stargz"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"golang.org/x/sys/unix"
)

func TestCheckInterval(t *testing.T) {
	var (
		largeInterval = time.Hour
		tr            = &calledRoundTripper{}
		c             = &connection{
			url: "test",
			tr:  tr,
		}
		fs = &filesystem{
			conn:               map[string]*connection{"test": c},
			layerValidInterval: largeInterval,
		}
	)

	check := func(name string) (time.Time, bool) {
		beforeUpdate := time.Now()

		time.Sleep(time.Millisecond)

		tr.called = false
		if err := fs.Check(context.TODO(), "test"); err != nil {
			t.Fatalf("%s: check mustn't be failed", name)
		}

		time.Sleep(time.Millisecond)

		afterUpdate := time.Now()
		if !tr.called {
			return c.lastCheck, false
		}
		if !(c.lastCheck.After(beforeUpdate) && c.lastCheck.Before(afterUpdate)) {
			t.Errorf("%s: updated time must be after %q and before %q but %q", name, beforeUpdate, afterUpdate, c.lastCheck)
		}

		return c.lastCheck, true
	}

	// first time
	firstTime, called := check("first time")
	if !called {
		t.Error("must be called for the first time")
	}

	// second time(not expired yet)
	secondTime, called := check("second time")
	if called {
		t.Error("mustn't be checked if not expired")
	}
	if !secondTime.Equal(firstTime) {
		t.Errorf("lastCheck time must be same as first time(%q) but %q", firstTime, secondTime)
	}

	// third time(expired, must be checked)
	c.lastCheck = time.Now().Add(-1 * largeInterval) // set to "largeInterval" ago
	if _, called := check("third time"); !called {
		t.Error("must be called for the third time")
	}
}

type calledRoundTripper struct {
	called bool
}

func (c *calledRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	c.called = true
	res = &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("test"))),
	}
	return
}

func TestCheck(t *testing.T) {
	tr := &breakRoundTripper{}
	fs := &filesystem{
		conn: map[string]*connection{
			"test": {
				url: "test",
				tr:  tr,
			},
		},
		layerValidInterval: 0,
	}
	tr.success = true
	if err := fs.Check(context.TODO(), "test"); err != nil {
		t.Errorf("connection failed; wanted to succeed")
	}

	tr.success = false
	if err := fs.Check(context.TODO(), "test"); err == nil {
		t.Errorf("connection succeeded; wanted to fail")
	}
}

type breakRoundTripper struct {
	success bool
}

func (b *breakRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	if b.success {
		res = &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     make(http.Header),
			Body:       ioutil.NopCloser(bytes.NewReader([]byte("test"))),
		}
	} else {
		res = &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
		}
	}
	return
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
						rr, status := f.Read(tmpbuf, offset)
						if status != fuse.OK {
							t.Errorf("failed to read off=%d, size=%d, filesize=%d: %v", offset, size, filesize, err)
							return
						}
						if rsize := rr.Size(); int64(rsize) != wantN {
							t.Errorf("read size: %d; want: %d", rsize, wantN)
							return
						}
						tmpbuf = make([]byte, len(tmpbuf))
						respData, status := rr.Bytes(tmpbuf)
						if status != fuse.OK {
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
	var attr fuse.Attr
	inode, status := rootNode.Lookup(&attr, testName, nil)
	if status != fuse.OK {
		t.Fatalf("failed to lookup test node; status: %v", status)
	}
	ni := inode.Node()
	node, ok := ni.(*node)
	if !ok {
		t.Fatalf("test node is invalid")
	}
	f, status := node.Open(0, nil)
	if status != fuse.OK {
		t.Fatalf("failed to open test file; status: %v", status)
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
			name: "state_file",
			in: []tarent{
				regfile("test", "test"),
			},
			want: []check{
				hasFileDigest("test", digestFor("test")),
				hasStateFile(testStateLayerDigest + ".json"),
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
	gr := &stargzReader{
		r:     r,
		cache: &testCache{membuf: map[string]string{}, t: t},
	}
	ur := &urlReaderAt{
		url:       "http://example.com/dummy/blobs/sha256/deadbeef",
		size:      64,
		chunkSize: 1024,
		cache:     &testCache{membuf: map[string]string{}, t: t},
	}
	rootNode := &node{
		Node: nodefs.NewDefaultNode(),
		gr:   gr,
		e:    root,
		s:    newState(testStateLayerDigest, ur),
	}
	_ = nodefs.NewFileSystemConnector(rootNode, &nodefs.Options{
		NegativeTimeout: 0,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		Owner:           nil, // preserve owners.
	})

	return rootNode
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

type xAttr map[string]string

func directory(name string, opts ...interface{}) tarent {
	if !strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("dir %q hasn't suffix /", name))
	}
	var xattrs xAttr
	for _, opt := range opts {
		if v, ok := opt.(xAttr); ok {
			xattrs = v
		}
	}
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name,
			Mode:     0755,
			Xattrs:   xattrs,
		},
	}
}

type check func(*testing.T, *node)

func fileNotExist(file string) check {
	return func(t *testing.T, root *node) {
		ent, inode, err := getDirentAndNode(root, file)
		if err == nil || ent != nil || inode != nil {
			t.Errorf("Node %q exists", file)
		}
	}
}

func hasFileDigest(file string, digest string) check {
	return func(t *testing.T, root *node) {
		_, inode, err := getDirentAndNode(root, file)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", file, err)
		}
		n, ok := inode.Node().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", file)
		}
		if n.e.Digest != digest {
			t.Fatalf("Digest(%q) = %q, want %q", file, n.e.Digest, digest)
		}
	}
}

func hasValidWhiteout(name string) check {
	return func(t *testing.T, root *node) {
		ent, inode, err := getDirentAndNode(root, name)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", name, err)
		}
		n, ok := inode.Node().(*whiteout)
		if !ok {
			t.Fatalf("entry %q isn't a whiteout node", name)
		}
		var a fuse.Attr
		if status := n.GetAttr(&a, nil, nil); status != fuse.OK {
			t.Fatalf("failed to get attributes of file %q: %v", name, status)
		}
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
		_, inode, err := getDirentAndNode(root, entry)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", entry, err)
		}
		n, ok := inode.Node().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", entry)
		}

		// check xattr exists in the xattrs list.
		attrs, status := n.ListXAttr(nil)
		if status != fuse.OK {
			t.Fatalf("failed to get xattrs list of node %q: %v", entry, err)
		}
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
		v, status := n.GetXAttr(name, nil)
		if status != fuse.OK {
			t.Fatalf("failed to get xattr %q of node %q: %v", name, entry, err)
		}
		if string(v) != value {
			t.Errorf("node %q has an invalid xattr %q; want %q", entry, v, value)
			return
		}
	}
}

func hasStateFile(id string) check {
	return func(t *testing.T, root *node) {

		// Check existence of state dir
		var attr fuse.Attr
		sti, status := root.Lookup(&attr, stateDirName, nil)
		if status != fuse.OK {
			t.Errorf("failed to lookup directory %q: %v", stateDirName, status)
			return
		}
		st, ok := sti.Node().(*state)
		if !ok {
			t.Errorf("directory %q isn't a state node", stateDirName)
			return
		}

		// Check existence of state file
		ents, status := st.OpenDir(nil)
		if status != fuse.OK {
			t.Errorf("failed to open directory %q: %v", stateDirName, status)
			return
		}
		var found bool
		for _, e := range ents {
			if e.Name == id {
				found = true
			}
		}
		if !found {
			t.Errorf("direntry %q not found in %q", id, stateDirName)
			return
		}
		inode, status := st.Lookup(&attr, id, nil)
		if status != fuse.OK {
			t.Errorf("failed to lookup node %q in %q: %v", id, stateDirName, status)
			return
		}
		n, ok := inode.Node().(*statFile)
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
		status = n.GetAttr(&attr, nil, nil)
		if status != fuse.OK {
			t.Errorf("failed to get attr of state file: %v", status)
			return
		}

		// get data via state file
		tmp := make([]byte, 4096)
		res, status := n.Read(nil, tmp, 0, nil)
		if status != fuse.OK {
			t.Errorf("failed to read state file: %v", status)
			return
		}
		gotState, status := res.Bytes(nil)
		if status != fuse.OK {
			t.Errorf("failed to get result bytes of state file: %v", status)
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
func getDirentAndNode(root *node, path string) (ent *fuse.DirEntry, n *nodefs.Inode, err error) {
	dir, base := filepath.Split(filepath.Clean(path))

	// get the target's parent directory.
	var attr fuse.Attr
	d := root
	for _, name := range strings.Split(dir, "/") {
		if len(name) == 0 {
			continue
		}
		di, status := d.Lookup(&attr, name, nil)
		if status != fuse.OK {
			err = fmt.Errorf("failed to lookup directory %q: %v", name, status)
			return
		}
		var ok bool
		if d, ok = di.Node().(*node); !ok {
			err = fmt.Errorf("directory %q isn't a normal node", name)
			return
		}

	}

	// get the target's direntry.
	var ents []fuse.DirEntry
	ents, status := d.OpenDir(nil)
	if status != fuse.OK {
		err = fmt.Errorf("failed to open directory %q: %v", path, status)
	}
	var found bool
	for _, e := range ents {
		if e.Name == base {
			ent, found = &e, true
		}
	}
	if !found {
		err = fmt.Errorf("direntry %q not found in the parent directory of %q", base, path)
	}

	// get the target's node.
	n, status = d.Lookup(&attr, base, nil)
	if status != fuse.OK {
		err = fmt.Errorf("failed to lookup node %q: %v", path, status)
	}

	return
}

func digestFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum)
}
