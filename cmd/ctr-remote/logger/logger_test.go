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

package logger

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

const (
	opaqueXattr      = "trusted.overlay.opaque"
	opaqueXattrValue = "y"
)

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
				hasFileContents("foo/bar.txt", "test"),
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
				hasFileContents("foo/bar.txt", "test"),
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inTar, cancelIn := buildTar(t, tt.in)
			defer cancelIn()
			inTarData, err := ioutil.ReadAll(inTar)
			if err != nil {
				t.Fatalf("failed to read input tar: %q", err)
			}
			root := newRoot(bytes.NewReader(inTarData), NewOpenReadMonitor())
			timeSec := time.Second
			fusefs.NewNodeFS(root, &fusefs.Options{
				AttrTimeout:     &timeSec,
				EntryTimeout:    &timeSec,
				NullPermissions: true,
			})
			if err := root.InitNodes(); err != nil {
				t.Fatalf("failed to init nodes: %v", err)
			}
			for _, want := range tt.want {
				want(t, root)
			}
		})
	}
}

type check func(*testing.T, *node)

func fileNotExist(file string) check {
	return func(t *testing.T, root *node) {
		_, err := getNode(t, root, file)
		if err == nil {
			t.Errorf("Node %q exists", file)
		}
	}
}

func hasFileContents(file string, want string) check {
	return func(t *testing.T, root *node) {
		inode, err := getNode(t, root, file)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", file, err)
		}
		n, ok := inode.Operations().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", file)
		}
		if n.r == nil {
			t.Fatalf("reader not found for file %q", file)
		}
		data := make([]byte, n.attr.Size)
		gotSize, err := n.r.ReadAt(data, 0)
		if uint64(gotSize) != n.attr.Size || (err != nil && err != io.EOF) {
			t.Errorf("failed to read %q: %v", file, err)
		}
		if string(data) != want {
			t.Errorf("Contents(%q) = %q, want %q", file, data, want)
		}
	}
}

func hasValidWhiteout(name string) check {
	return func(t *testing.T, root *node) {
		inode, err := getNode(t, root, name)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", name, err)
		}
		n, ok := inode.Operations().(*whiteout)
		if !ok {
			t.Fatalf("entry %q isn't a whiteout node", name)
		}
		var ao fuse.AttrOut
		if errno := n.Operations().(fusefs.NodeGetattrer).Getattr(context.Background(), nil, &ao); errno != 0 {
			t.Fatalf("failed to get attributes of file %q: %v", name, errno)
		}

		// validate the node
		a := ao.Attr
		if a.Mode&syscall.S_IFCHR != syscall.S_IFCHR {
			t.Errorf("whiteout node %q isn't a char device %q but %q",
				name, strconv.FormatUint(uint64(syscall.S_IFCHR), 2), strconv.FormatUint(uint64(a.Mode), 2))
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
		inode, err := getNode(t, root, entry)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", entry, err)
		}
		n, ok := inode.Operations().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", entry)
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

func getNode(t *testing.T, root *node, path string) (n *fusefs.Inode, err error) {
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

	// get the target's node.
	node, errno := d.Lookup(context.Background(), base, &eo)
	if errno != 0 {
		return nil, fmt.Errorf("failed to lookup node %q: %v", path, errno)
	}

	return node, nil
}

func TestOpenRead(t *testing.T) {
	tests := []struct {
		name string
		in   []tarent
		do   accessFunc
		want []string
	}{
		{
			name: "noopt",
			in: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			},
			do:   doAccess(),
			want: []string{},
		},
		{
			name: "open_and_read",
			in: []tarent{
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			},
			do: doAccess(
				openFile("bar/baa.txt"),
				readFile("bar/baz.txt", make([]byte, 3)),
			),
			want: []string{
				"bar/baa.txt", // open
				"bar/baz.txt", // open for read
				"bar/baz.txt", // read
			},
		},
		{
			name: "hardlink",
			in: []tarent{
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				hardlink("bar.txt", "baz.txt"),
				regfile("baa.txt", "baa"),
			},
			do: doAccess(
				readFile("bar.txt", make([]byte, 3)),
			),
			want: []string{
				"baz.txt", // open for read; must be original file
				"baz.txt", // read; must be original file
			},
		},
		{
			name: "symlink",
			in: []tarent{
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				symlink("bar.txt", "baz.txt"),
				regfile("baa.txt", "baa"),
			},
			do: doAccess(
				readFile("bar.txt", make([]byte, 3)),
			),
			want: []string{
				"baz.txt", // open for read; must be original file
				"baz.txt", // read; must be original file
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Prepare input tar file
			inTar, cancelIn := buildTar(t, tt.in)
			defer cancelIn()
			inTarData, err := ioutil.ReadAll(inTar)
			if err != nil {
				t.Fatalf("failed to read input tar: %q", err)
			}

			dir, err := ioutil.TempDir("", "loggertest")
			if err != nil {
				t.Fatalf("failed to prepare temp directory")
			}
			defer os.RemoveAll(dir)

			monitor := NewOpenReadMonitor()
			cleanup, err := Mount(dir, bytes.NewReader(inTarData), monitor)
			if err != nil {
				t.Fatalf("failed to mount at %q: %q", dir, err)
			}
			defer cleanup()

			if err := tt.do(dir); err != nil {
				t.Fatalf("failed to do specified operations: %q", err)
			}

			if err := cleanup(); err != nil {
				t.Logf("failed to unmount: %v", err)
			}

			log := monitor.DumpLog()
			for i, l := range log {
				t.Logf("  [%d]: %s", i, l)
			}
			if len(log) != len(tt.want) {
				t.Errorf("num of log: got %d; want %d", len(log), len(tt.want))
				return
			}
			for i, l := range log {
				if l != tt.want[i] {
					t.Errorf("log: got %q; want %q", l, tt.want[i])
					return
				}
			}
		})
	}
}

func buildTar(t *testing.T, ents []tarent) (r io.Reader, cancel func()) {
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
	return pr, func() { go pr.Close(); go pw.Close() }
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

func hardlink(name string, linkname string) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeLink,
			Name:     name,
			Mode:     0644,
			Linkname: linkname,
		},
	}
}

func symlink(name string, linkname string) tarent {
	return tarent{
		header: &tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     name,
			Mode:     0644,
			Linkname: linkname,
		},
	}
}

type accessFunc func(basepath string) error

func doAccess(ac ...accessFunc) accessFunc {
	return func(basepath string) error {
		for _, a := range ac {
			if err := a(basepath); err != nil {
				return err
			}
		}
		return nil
	}
}

func openFile(filename string) accessFunc {
	return func(basepath string) error {
		f, err := os.Open(filepath.Join(basepath, filename))
		if err != nil {
			return err
		}
		f.Close()
		return nil
	}
}

func readFile(filename string, b []byte) accessFunc {
	return func(basepath string) error {
		f, err := os.Open(filepath.Join(basepath, filename))
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Read(b); err != nil {
			if err != io.EOF {
				return err
			}
		}
		return nil
	}
}
