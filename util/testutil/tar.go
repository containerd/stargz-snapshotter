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

// This utility helps test codes to generate sample tar blobs.

import (
	"archive/tar"
	"fmt"
	"io"
	"strings"
	"time"
)

// TarEntry is an entry of tar.
type TarEntry interface {
	AppendTar(tw *tar.Writer, opts Options) error
}

// Option is a set of options used during building blob.
type Options struct {

	// Prefix is the prefix string need to be added to each file name (e.g. "./", "/", etc.)
	Prefix string
}

// Options is an option used during building blob.
type Option func(o *Options)

// WithPrefix is an option to add a prefix string to each file name (e.g. "./", "/", etc.)
func WithPrefix(prefix string) Option {
	return func(o *Options) {
		o.Prefix = prefix
	}
}

// BuildTar builds a tar blob
func BuildTar(ents []TarEntry, opts ...Option) io.Reader {
	var bo Options
	for _, o := range opts {
		o(&bo)
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, ent := range ents {
			if err := ent.AppendTar(tw, bo); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return pr
}

type tarEntryFunc func(*tar.Writer, Options) error

func (f tarEntryFunc) AppendTar(tw *tar.Writer, opts Options) error { return f(tw, opts) }

// DirecoryOption is an option for a directory entry.
type DirectoryOption func(o *dirOpts)

type dirOpts struct {
	uid int
	gid int
}

// WithDirOwner specifies the owner of the directory.
func WithDirOwner(uid, gid int) DirectoryOption {
	return func(o *dirOpts) {
		o.uid = uid
		o.gid = gid
	}
}

// Dir is a directory entry
func Dir(name string, opts ...DirectoryOption) TarEntry {
	return tarEntryFunc(func(tw *tar.Writer, buildOpts Options) error {
		var dOpts dirOpts
		for _, o := range opts {
			o(&dOpts)
		}
		if !strings.HasSuffix(name, "/") {
			panic(fmt.Sprintf("missing trailing slash in dir %q ", name))
		}
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     buildOpts.Prefix + name,
			Mode:     0755,
			Uid:      dOpts.uid,
			Gid:      dOpts.gid,
		})
	})
}

// FileOption is an option for a file entry.
type FileOption func(o *fileOpts)

type fileOpts struct {
	uid    int
	gid    int
	xattrs map[string]string
}

// WithFileOwner specifies the owner of the file.
func WithFileOwner(uid, gid int) FileOption {
	return func(o *fileOpts) {
		o.uid = uid
		o.gid = gid
	}
}

// WithFileXattrs specifies the extended attributes of the file.
func WithFileXattrs(xattrs map[string]string) FileOption {
	return func(o *fileOpts) {
		o.xattrs = xattrs
	}
}

// File is a regilar file entry
func File(name, contents string, opts ...FileOption) TarEntry {
	return tarEntryFunc(func(tw *tar.Writer, buildOpts Options) error {
		var fOpts fileOpts
		for _, o := range opts {
			o(&fOpts)
		}
		if strings.HasSuffix(name, "/") {
			return fmt.Errorf("bogus trailing slash in file %q", name)
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     buildOpts.Prefix + name,
			Mode:     0644,
			Xattrs:   fOpts.xattrs,
			Size:     int64(len(contents)),
			Uid:      fOpts.uid,
			Gid:      fOpts.gid,
		}); err != nil {
			return err
		}
		_, err := io.WriteString(tw, contents)
		return err
	})
}

// Symlink is a symlink entry
func Symlink(name, target string) TarEntry {
	return tarEntryFunc(func(tw *tar.Writer, buildOpts Options) error {
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     buildOpts.Prefix + name,
			Linkname: target,
			Mode:     0644,
		})
	})
}

// Link is a hard-link entry
func Link(name, linkname string) TarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, buildOpts Options) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeLink,
			Name:       buildOpts.Prefix + name,
			Linkname:   linkname,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}

// Chardev is a character device entry
func Chardev(name string, major, minor int64) TarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, buildOpts Options) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeChar,
			Name:       buildOpts.Prefix + name,
			Devmajor:   major,
			Devminor:   minor,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}

// Blockdev is a block device entry
func Blockdev(name string, major, minor int64) TarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, buildOpts Options) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeBlock,
			Name:       buildOpts.Prefix + name,
			Devmajor:   major,
			Devminor:   minor,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}

// Fifo is a fifo entry
func Fifo(name string) TarEntry {
	now := time.Now()
	return tarEntryFunc(func(w *tar.Writer, buildOpts Options) error {
		return w.WriteHeader(&tar.Header{
			Typeflag:   tar.TypeFifo,
			Name:       buildOpts.Prefix + name,
			ModTime:    now,
			AccessTime: now,
			ChangeTime: now,
		})
	})
}
