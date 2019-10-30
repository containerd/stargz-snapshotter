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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/containerd/containerd/plugin"
	"github.com/google/crfs/stargz"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/ktock/remote-snapshotter/cache"
	fsplugin "github.com/ktock/remote-snapshotter/filesystems"
	"golang.org/x/sys/unix"
)

const (
	blockSize             = 512
	defaultCacheChunkSize = 50000
	defaultPrefetchSize   = 5000000
	directoryCacheType    = "directory"
	memoryCacheType       = "memory"
)

type Config struct {
	Insecure       []string `toml:"insecure"`
	CacheChunkSize int64    `toml:"cache_chunk_size"`
	PrefetchSize   int64    `toml:"prefetch_size"`
	HTTPCacheType  string   `toml:"http_cache_type"`
	FSCacheType    string   `toml:"filesystem_cache_type"`
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
			ic.Meta.Exports["root"] = ic.Root

			return NewFilesystem(ic.Root, config)
		},
	})
}

func NewFilesystem(root string, config *Config) (fs *filesystem, err error) {
	fs = &filesystem{
		insecure:       config.Insecure,
		cacheChunkSize: config.CacheChunkSize,
		prefetchSize:   config.PrefetchSize,
	}
	if fs.cacheChunkSize == 0 {
		fs.cacheChunkSize = defaultCacheChunkSize
	}
	if fs.prefetchSize == 0 {
		fs.prefetchSize = defaultPrefetchSize
	}
	fs.httpCache, err = getCache(config.HTTPCacheType, filepath.Join(root, "httpcache"))
	if err != nil {
		return nil, err
	}
	fs.fsCache, err = getCache(config.FSCacheType, filepath.Join(root, "fscache"))
	if err != nil {
		return nil, err
	}

	return
}

type filesystem struct {
	insecure       []string
	cacheChunkSize int64
	prefetchSize   int64
	httpCache      cache.BlobCache
	fsCache        cache.BlobCache
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

func (m *mounter) Prepare(ref, digest string) error {

	// Resolve the reference and authenticate using ~/.docker/config.json.
	url, tr, err := m.resolve(ref, digest)
	if err != nil {
		return fmt.Errorf("failed to resolve the reference")
	}

	// Get size information.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return err
	}
	req.WithContext(ctx)
	res, err := tr.RoundTrip(req)
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
		url:       url,
		t:         tr,
		size:      layerSize,
		chunkSize: m.fs.cacheChunkSize,
		cache:     m.fs.httpCache,
	}, 0, layerSize)
	r, err := stargz.Open(sr)
	if err != nil {
		return err
	}
	root, ok := r.Lookup("")
	if !ok {
		return fmt.Errorf("failed to get a TOCEntry of the root node")
	}
	gr := &stargzReader{
		digest: digest,
		r:      r,
		cache:  m.fs.fsCache,
	}
	if err := gr.prefetch(sr, m.fs.prefetchSize); err != nil {
		return err
	}
	m.root = &node{
		Node: nodefs.NewDefaultNode(),
		gr:   gr,
		e:    root,
	}
	return nil
}

func (m *mounter) Mount(target string) error {
	root := m.root
	conn := nodefs.NewFileSystemConnector(root, &nodefs.Options{
		NegativeTimeout: 0,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		Owner:           nil, // preserve owners.
	})
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

func (m *mounter) isInsecure(host string) bool {
	for _, i := range m.fs.insecure {
		if ok, _ := regexp.Match(i, []byte(host)); ok {
			return true
		}
	}

	return false
}

func (m *mounter) resolve(ref, digest string) (url string, tr http.RoundTripper, err error) {

	// Parse the reference and make an URL of the blob.
	refs := strings.Split(ref, "/")
	if len(refs) < 2 {
		return "", nil, fmt.Errorf("invalid reference format %q", ref)
	}
	host, path := refs[0], strings.Join(refs[1:], "/")
	imagetag := strings.Split(path, ":")
	if len(imagetag) <= 0 {
		return "", nil, fmt.Errorf("image tag hasn't been specified.")
	}
	image := imagetag[0]
	scheme := "https"
	if m.isInsecure(host) {
		scheme = "http"
	}
	url = fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, host, image, digest)

	// Authenticate.
	var opts []name.Option
	if m.isInsecure(host) {
		opts = append(opts, name.Insecure)
	}
	nameref, err := name.ParseReference(ref, opts...)
	if err != nil {
		return "", nil, err
	}
	auth, err := authn.DefaultKeychain.Resolve(nameref.Context()) // Authn against the repository.
	if err != nil {
		return "", nil, err
	}
	tr, err = transport.New(nameref.Context().Registry, auth, http.DefaultTransport, []string{nameref.Scope(transport.PullScope)})
	if err != nil {
		return "", nil, err
	}

	// Redirect if necessary(GCR specific).
	//
	// GCR serves layer blobs with a redirect only on GET. So we get the
	// destination Location of the layer using GET with Range to avoid fetching
	// whole blob data.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", nil, err
	}
	req.WithContext(ctx)
	req.Header.Set("Range", "bytes=0-1")
	if res, err := tr.RoundTrip(req); err == nil && res.StatusCode < 400 {
		defer res.Body.Close()
		if redir := res.Header.Get("Location"); redir != "" && res.StatusCode/100 == 3 {
			url = redir
		}
	}

	return url, tr, nil
}

// filesystem nodes
type node struct {
	nodefs.Node
	gr *stargzReader
	e  *stargz.TOCEntry
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
	return n.Inode().NewChild(name, ce.Stat().IsDir(), &node{
		Node: nodefs.NewDefaultNode(),
		gr:   n.gr,
		e:    ce,
	}), entryToAttr(ce, out)
}

func (n *node) Deletable() bool {
	// read-only filesystem
	return false
}

func (n *node) OnForget() {
	// Do nothing.
}

func (n *node) Access(mode uint32, context *fuse.Context) (code fuse.Status) {
	if context.Owner.Uid == 0 { // root can do anything.
		return fuse.OK
	}
	if mode == 0 { // Requires nothing.
		return fuse.OK
	}

	var shift uint32
	if uint32(n.e.Uid) == context.Owner.Uid {
		shift = 6
	} else if uint32(n.e.Gid) == context.Owner.Gid {
		shift = 3
	} else {
		shift = 0
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
	ra, err := n.gr.openFile(n.e.Name)
	if err != nil {
		return nil, fuse.EIO
	}
	return &file{
		File: nodefs.NewDefaultFile(),
		n:    n,
		e:    n.e,
		ra:   ra,
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

func (n *node) GetXAttr(attribute string, context *fuse.Context) (data []byte, code fuse.Status) {
	v, ok := n.e.Xattrs[attribute]
	if !ok {
		return nil, fuse.ENOATTR
	}
	return v, fuse.OK
}

func (n *node) ListXAttr(ctx *fuse.Context) (attrs []string, code fuse.Status) {
	for k, _ := range n.e.Xattrs {
		attrs = append(attrs, k)
	}
	return attrs, fuse.OK
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
	ra io.ReaderAt
}

func (f *file) String() string {
	return "stargzFile"
}

func (f *file) Read(buf []byte, off int64) (fuse.ReadResult, fuse.Status) {
	if _, err := f.ra.ReadAt(buf, off); err != nil {
		return nil, fuse.EIO
	}
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
	out.Nlink = uint32(e.NumLink)
	if out.Nlink == 0 {
		out.Nlink = 1 // zero "NumLink" means one.
	}
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

func getCache(ctype, dir string) (cache.BlobCache, error) {
	switch ctype {
	case directoryCacheType:
		return cache.NewDirectoryCache(dir)
	case memoryCacheType:
		return cache.NewMemoryCache(), nil
	default:
		return cache.NewNopCache(), nil
	}
}
