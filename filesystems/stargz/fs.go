// +build linux

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

//
// Example implementation of FileSystem.
//
// This implementation uses stargz by CRFS(https://github.com/google/crfs) as
// image format, which has following feature:
// - We can use docker registry as a backend store (means w/o additional layer
//   stores).
// - The stargz-formatted image is still docker-compatible (means normal
//   runtimes can still use the formatted image).
//
// Currently, we reimplemented CRFS-like filesystem for ease of integration.
// But in the near future, we intend to integrate it with CRFS.
//

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/containerd/containerd/plugin"
	"github.com/google/crfs/stargz"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"golang.org/x/sys/unix"

	fsplugin "github.com/ktock/remote-snapshotter/filesystems"
)

const (
	blockSize          uint32 = 512
	stargzHardlinkType string = "hardlink" // ad-hoc
)

type Config struct {
	Insecure []string `toml:"insecure"`
}

func init() {
	plugin.Register(&plugin.Registration{
		Type:   fsplugin.RemoteFileSystemPlugin,
		Config: &Config{},
		ID:     "stargz",
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, fmt.Errorf("invalid stargz configuration")
			}

			return &filesystem{config.Insecure}, nil
		},
	})
}

type filesystem struct {
	insecure []string
}

func (fs *filesystem) Mounter() fsplugin.Mounter {
	return &mounter{
		fs: fs,
	}
}

type mounter struct {
	root nodefs.Node
	fs   *filesystem
}

// TODO: schema, auth config with config files.
func (m *mounter) Prepare(ref, digest string) error {

	// Parse host, owner and imagetag.
	refs := strings.Split(ref, "/")
	if len(refs) < 2 {
		return fmt.Errorf("invalid reference format %s", ref)
	}
	host, path := refs[0], strings.Join(refs[1:], "/")
	imagetag := strings.Split(path, ":")
	if len(imagetag) <= 0 {
		return fmt.Errorf("image tag hasn't been specified.")
	}
	image := imagetag[0]
	url := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", m.scheme(host), host, image, digest)

	// See if the layer is served with a redirect(GCR specific).
	//
	// GCR serves layer blobs with a redirect only on GET. So we get the
	// destination Location of the layer using GET with Range to avoid fetching
	// whole blob data.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.WithContext(ctx)
	req.Header.Set("Range", "bytes=0-1")
	res, err := http.DefaultTransport.RoundTrip(req) // NOT DefaultClient; don't want redirects
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("invalid status code from the registry: %v", res.StatusCode)
	}
	if redir := res.Header.Get("Location"); redir != "" && res.StatusCode/100 == 3 {
		url = redir
	}

	// Get size information
	req, err = http.NewRequest("HEAD", url, nil)
	if err != nil {
		return err
	}
	req.WithContext(ctx)
	res, err = http.DefaultTransport.RoundTrip(req) // NOT DefaultClient; don't want redirects
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to request size using HEAD")
	}
	sizeRaw := res.Header.Get("Content-Length")
	if sizeRaw == "" {
		return fmt.Errorf("failed to get size information through Content-Length")
	}
	layerSize, err := strconv.ParseInt(sizeRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse size")
	}

	// Try to get stargz reader.
	sr := io.NewSectionReader(&urlReaderAt{
		url: url,
		t:   http.DefaultTransport,
	}, 0, layerSize)
	r, err := stargz.Open(sr)
	if err != nil {
		return err
	}
	root, ok := r.Lookup("")
	if !ok {
		return fmt.Errorf("failed to get a TOCEntry of the root node")
	}
	m.root = &node{
		Node: nodefs.NewDefaultNode(),
		r:    r,
		e:    root,
	}
	return nil
}

func (m *mounter) Mount(target string) error {
	root := m.root
	conn := nodefs.NewFileSystemConnector(root, nil)
	server, err := fuse.NewServer(conn.RawFS(), target, &fuse.MountOptions{})
	// server.SetDebug(true)
	if err != nil {
		return fmt.Errorf("Mount failed: %v\n", err)
	}
	go server.Serve()
	err = server.WaitMount()
	if err != nil {
		return err
	}
	return nil
}

func (m *mounter) scheme(host string) string {
	for _, i := range m.fs.insecure {
		if ok, _ := regexp.Match(i, []byte(host)); ok {
			return "http"
		}
	}

	return "https"
}

// TODO: cache, prefetch
type urlReaderAt struct {
	url string
	t   http.RoundTripper
}

func (r *urlReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("GET", r.url, nil)
	if err != nil {
		return 0, err
	}
	req = req.WithContext(ctx)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(p))-1)
	req.Header.Add("Range", rangeHeader)
	req.Header.Add("Accept-Encoding", "identity")
	req.Close = false
	res, err := r.t.RoundTrip(req) // NOT DefaultClient; don't want redirects
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("unexpected status code on %s: %v", r.url, res.Status)
	}
	return io.ReadFull(res.Body, p)
}

// filesystem nodes
type node struct {
	nodefs.Node
	r *stargz.Reader
	e *stargz.TOCEntry
}

func (n *node) OnMount(conn *nodefs.FileSystemConnector) {
	// Do nothing.
}

func (n *node) OnUnmount() {
	// Do nothing.
}

func (n *node) Lookup(out *fuse.Attr, name string, context *fuse.Context) (*nodefs.Inode, fuse.Status) {
	c := n.Inode().GetChild(name)
	if c != nil {
		s := c.Node().GetAttr(out, nil, context)
		if s != fuse.OK {
			return nil, s
		}
		return c, fuse.OK
	}

	ce, ok := n.e.LookupChild(name)
	if !ok {
		return nil, fuse.ENOENT
	}
	ce, ok = n.traverseLink(ce)
	if !ok {
		return nil, fuse.EIO
	}
	return n.Inode().NewChild(name, ce.Stat().IsDir(), &node{
		Node: nodefs.NewDefaultNode(),
		r:    n.r,
		e:    ce,
	}), entryToAttr(ce, out)
}

func (n *node) traverseLink(e *stargz.TOCEntry) (*stargz.TOCEntry, bool) {
	if e.Type != stargzHardlinkType {
		return e, true
	}
	ce, ok := n.r.Lookup(e.LinkName)
	if !ok {
		return nil, false
	}
	return n.traverseLink(ce)
}

func (n *node) Deletable() bool {
	// read-only filesystem
	return false
}

func (n *node) OnForget() {
	// Do nothing.
}

func (n *node) Access(mode uint32, context *fuse.Context) (code fuse.Status) {
	var shift uint32
	if uint32(n.e.Uid) == context.Owner.Uid {
		shift = 0
	} else if uint32(n.e.Gid) == context.Owner.Gid {
		shift = 3
	} else {
		shift = 6
	}
	if mode<<shift&fileModeToSystemMode(n.e.Stat().Mode()) != 0 {
		return fuse.OK
	}

	return fuse.EPERM
}

func (n *node) Readlink(c *fuse.Context) ([]byte, fuse.Status) {
	return []byte(n.e.LinkName), fuse.OK
}

func (n *node) Open(flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	sr, err := n.r.OpenFile(n.e.Name)
	if err != nil {
		return nil, fuse.EIO
	}
	return &file{
		File: nodefs.NewDefaultFile(),
		n:    n,
		e:    n.e,
		sr:   sr,
	}, fuse.OK
}

func (n *node) OpenDir(context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	var ents []fuse.DirEntry
	n.e.ForeachChild(func(baseName string, ent *stargz.TOCEntry) bool {
		ents = append(ents, fuse.DirEntry{
			Mode: fileModeToSystemMode(ent.Stat().Mode()),
			Name: baseName,
			Ino:  inodeOfEnt(ent),
		})
		return true
	})
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	return ents, fuse.OK
}

func (n *node) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) (code fuse.Status) {
	return entryToAttr(n.e, out)
}

func (n *node) StatFs() *fuse.StatfsOut {

	// http://man7.org/linux/man-pages/man2/statfs.2.html
	return &fuse.StatfsOut{
		Blocks:  0, // dummy
		Bfree:   0,
		Bavail:  0,
		Files:   0, // dummy
		Ffree:   0,
		Bsize:   blockSize,
		NameLen: 1<<32 - 1,
		Frsize:  blockSize,
		Padding: 0,
		Spare:   [6]uint32{},
	}
}

type file struct {
	nodefs.File
	n  *node
	e  *stargz.TOCEntry
	sr *io.SectionReader
}

func (f *file) String() string {
	return "stargzFile"
}

func (f *file) Read(buf []byte, off int64) (fuse.ReadResult, fuse.Status) {
	n := f.n

	// Read file-relative fixed-size chunks from the tar file,
	// then return required part of the chunks.
	remain := int64(n.e.Stat().Size()) - off
	if remain < 0 {
		remain = 0
	}
	nr := 0
	for nr < len(buf) {
		ce, ok := n.r.ChunkEntryForOffset(n.e.Name, off+int64(nr))
		if !ok || !(ce.ChunkOffset <= off && off < ce.ChunkOffset+ce.ChunkSize) {
			break
		}
		// TODO: cache
		data := make([]byte, int(ce.ChunkSize))
		if _, err := f.sr.ReadAt(data, ce.ChunkOffset); err != nil {
			if err != io.EOF {
				return nil, fuse.EIO
			}
		}
		if ce.ChunkOffset < off && off < ce.ChunkOffset+ce.ChunkSize {
			roff := off - ce.ChunkOffset + int64(nr)
			if roff > int64(len(data)) {
				roff = int64(len(data))
			}
			data = data[off:]
		}
		n := copy(buf[nr:], data)
		nr += n
	}
	buf = buf[:nr]

	return fuse.ReadResultData(buf), fuse.OK
}

func (f *file) GetAttr(out *fuse.Attr) fuse.Status {
	return entryToAttr(f.e, out)
}

func inodeOfEnt(e *stargz.TOCEntry) uint64 {
	return uint64(uintptr(unsafe.Pointer(e)))
}

func entryToAttr(e *stargz.TOCEntry, out *fuse.Attr) (code fuse.Status) {
	fi := e.Stat()
	out.Ino = inodeOfEnt(e)
	out.Size = uint64(fi.Size())
	out.Blksize = blockSize
	out.Blocks = out.Size / uint64(out.Blksize)
	if out.Size%uint64(out.Blksize) > 0 {
		out.Blocks++
	}
	out.Mtime = uint64(fi.ModTime().Unix())
	out.Mtimensec = uint32(fi.ModTime().UnixNano())
	out.Mode = fileModeToSystemMode(fi.Mode())
	out.Owner = fuse.Owner{Uid: uint32(e.Uid), Gid: uint32(e.Gid)}
	out.Rdev = uint32(unix.Mkdev(uint32(e.DevMajor), uint32(e.DevMinor)))
	out.Nlink = 1   // TODO
	out.Padding = 0 // TODO

	return fuse.OK
}

func fileModeToSystemMode(m os.FileMode) uint32 {

	// Convert os.FileMode to system's native bitmap.
	sm := uint32(m & 0777)
	switch m & os.ModeType {
	case os.ModeDevice:
		sm |= syscall.S_IFBLK
	case os.ModeDevice | os.ModeCharDevice:
		sm |= syscall.S_IFCHR
	case os.ModeDir:
		sm |= syscall.S_IFDIR
	case os.ModeNamedPipe:
		sm |= syscall.S_IFIFO
	case os.ModeSymlink:
		sm |= syscall.S_IFLNK
	case os.ModeSocket:
		sm |= syscall.S_IFSOCK
	default: // regular file.
		sm |= syscall.S_IFREG
	}
	if m&os.ModeSetgid != 0 {
		sm |= syscall.S_ISGID
	}
	if m&os.ModeSetuid != 0 {
		sm |= syscall.S_ISUID
	}
	if m&os.ModeSticky != 0 {
		sm |= syscall.S_ISVTX
	}

	return sm
}
