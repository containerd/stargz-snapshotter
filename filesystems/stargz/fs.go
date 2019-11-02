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
	whiteoutPrefix        = ".wh."
	whiteoutOpaqueDir     = whiteoutPrefix + whiteoutPrefix + ".opq"
	opaqueXattr           = "trusted.overlay.opaque"
	opaqueXattrValue      = "y"
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

// getCache gets a cache corresponding to specified type.
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
	host, url, err := m.parseReference(ref, digest)
	if err != nil {
		return fmt.Errorf("failed to parse the reference %q: %v", ref, err)
	}

	// authenticate to the registry using ~/.docker/config.json.
	url, tr, err := m.resolve(host, url, ref)
	if err != nil {
		return fmt.Errorf("failed to resolve the reference %q: %v", ref, err)
	}

	// Get size information.
	size, err := m.getSize(tr, url)
	if err != nil {
		return fmt.Errorf("failed to get layer size information from %q: %v", url, err)
	}

	// Construct filesystem from the remote stargz layer.
	sr := io.NewSectionReader(&urlReaderAt{
		url:       url,
		t:         tr,
		size:      size,
		chunkSize: m.fs.cacheChunkSize,
		cache:     m.fs.httpCache,
	}, 0, size)
	r, err := stargz.Open(sr)
	if err != nil {
		return fmt.Errorf("failed to parse stargz at %q: %v", url, err)
	}
	root, ok := r.Lookup("")
	if !ok {
		return fmt.Errorf("failed to get a TOCEntry of the root node of layer %q", url)
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
	if err != nil {
		return fmt.Errorf("failed to make server: %v", err)
	}
	// server.SetDebug(true) // TODO: make configurable with /etc/containerd/config.toml
	go server.Serve()
	return server.WaitMount()
}

// parseReference parses reference and digest then makes an URL of the blob.
// "ref" must be formatted in <host>/<image>[:<tag>] (<tag> is optional).
// "diegest" is an OCI's sha256 digest and must be prefixed by "sha256:".
func (m *mounter) parseReference(ref, digest string) (host string, url string, err error) {
	refs := strings.Split(ref, "/")
	if len(refs) < 2 {
		err = fmt.Errorf("reference must contain names of host and image but got %q", ref)
		return
	}
	scheme, host, image := "https", refs[0], strings.Split(strings.Join(refs[1:], "/"), ":")[0]
	if m.isInsecure(host) {
		scheme = "http"
	}
	return host, fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, host, image, digest), nil
}

// isInsecure checks if the specified host is registerd as "insecure" registry
// in this filesystem. If so, this filesystem treat the host in a proper way
// e.g. using HTTP instead of HTTPS.
func (m *mounter) isInsecure(host string) bool {
	for _, i := range m.fs.insecure {
		if ok, _ := regexp.Match(i, []byte(host)); ok {
			return true
		}
	}

	return false
}

// resolve resolves specified url and ref by authenticating and dealing with
// redirection in a proper way. We use `~/.docker/config.json` for authn.
func (m *mounter) resolve(host, url, ref string) (string, http.RoundTripper, error) {
	var opts []name.Option
	if m.isInsecure(host) {
		opts = append(opts, name.Insecure)
	}
	nameref, err := name.ParseReference(ref, opts...)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse reference %q: %v", ref, err)
	}

	// Authn against the repository using `~/.docker/config.json`
	auth, err := authn.DefaultKeychain.Resolve(nameref.Context())
	if err != nil {
		return "", nil, fmt.Errorf("failed to resolve the reference %q: %v", ref, err)
	}
	tr, err := transport.New(nameref.Context().Registry, auth, http.DefaultTransport, []string{nameref.Scope(transport.PullScope)})
	if err != nil {
		return "", nil, fmt.Errorf("faild to get transport of %q: %v", ref, err)
	}

	// Redirect if necessary(GCR specific). GCR serves layer blobs with a redirect
	// only on GET. So we get the destination Location of the layer using GET with
	// Range to avoid fetching whole blob data.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to request to the registry of %q: %v", url, err)
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

// getSize fetches the size info of the specified layer by requesting HEAD.
func (m *mounter) getSize(tr http.RoundTripper, url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	req.WithContext(ctx)
	res, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed HEAD request with code %v", res.StatusCode)
	}
	return strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
}

// node is a filesystem inode abstruction which implements node in go-fuse.
type node struct {
	nodefs.Node
	gr     *stargzReader
	e      *stargz.TOCEntry
	opaque bool // true if this node is an overlayfs opaque directory
}

func (n *node) OpenDir(context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	var ents []fuse.DirEntry
	whiteouts := map[string]*stargz.TOCEntry{}
	normalEnts := map[string]bool{}
	n.e.ForeachChild(func(baseName string, ent *stargz.TOCEntry) bool {

		// We don't want to show whiteouts.
		if strings.HasPrefix(baseName, whiteoutPrefix) {
			if baseName == whiteoutOpaqueDir {
				return true
			}
			// Add the overlayfs-compiant whiteout later.
			whiteouts[baseName] = ent
			return true
		}

		// This is a normal entry.
		normalEnts[baseName] = true
		ents = append(ents, fuse.DirEntry{
			Mode: fileModeToSystemMode(ent.Stat().Mode()),
			Name: baseName,
			Ino:  inodeOfEnt(ent),
		})
		return true
	})

	// Append whiteouts if no entry replaces the target entry in the lower layer.
	for w, ent := range whiteouts {
		if ok := normalEnts[w[len(whiteoutPrefix):]]; !ok {
			ents = append(ents, fuse.DirEntry{
				Mode: syscall.S_IFCHR,
				Name: w[len(whiteoutPrefix):],
				Ino:  inodeOfEnt(ent),
			})

		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	return ents, fuse.OK
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

	// We don't want to show whiteouts.
	if strings.HasPrefix(name, whiteoutPrefix) {
		return nil, fuse.ENOENT
	}

	ce, ok := n.e.LookupChild(name)
	if !ok {
		// If the entry exists as a whiteout, show an overlayfs-styled whiteout node.
		if wh, ok := n.e.LookupChild(fmt.Sprintf("%s%s", whiteoutPrefix, name)); ok {
			return n.Inode().NewChild(name, false, &whiteout{
				Node: nodefs.NewDefaultNode(),
				oe:   wh,
			}), entryToWhAttr(wh, out)
		}
		return nil, fuse.ENOENT
	}
	var opaque bool
	if _, ok := ce.LookupChild(whiteoutOpaqueDir); ok {
		// This entry is an opaque directory so make it recognizable for overlayfs.
		opaque = true
	}
	return n.Inode().NewChild(name, ce.Stat().IsDir(), &node{
		Node:   nodefs.NewDefaultNode(),
		gr:     n.gr,
		e:      ce,
		opaque: opaque,
	}), entryToAttr(ce, out)
}

func (n *node) Access(mode uint32, context *fuse.Context) fuse.Status {
	if context.Owner.Uid == 0 {
		// root can do anything.
		return fuse.OK
	}
	if mode == 0 {
		// Requires nothing.
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

func (n *node) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) fuse.Status {
	return entryToAttr(n.e, out)
}

func (n *node) GetXAttr(attribute string, context *fuse.Context) ([]byte, fuse.Status) {
	if attribute == opaqueXattr && n.opaque {
		// This node is an opaque directory so give overlayfs-compliant indicator.
		return []byte(opaqueXattrValue), fuse.OK
	}
	if v, ok := n.e.Xattrs[attribute]; ok {
		return v, fuse.OK
	}
	return nil, fuse.ENOATTR
}

func (n *node) ListXAttr(ctx *fuse.Context) (attrs []string, code fuse.Status) {
	if n.opaque {
		// This node is an opaque directory so add overlayfs-compliant indicator.
		attrs = append(attrs, opaqueXattr)
	}
	for k, _ := range n.e.Xattrs {
		attrs = append(attrs, k)
	}
	return attrs, fuse.OK
}

func (n *node) Readlink(c *fuse.Context) ([]byte, fuse.Status) {
	return []byte(n.e.LinkName), fuse.OK
}
func (n *node) Deletable() bool {
	// read-only filesystem
	return false
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

// file is a file abstruction which implements file in go-fuse.
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

// whiteout is a whiteout abstruction compliant to overlayfs. This implements
// node in go-fuse.
type whiteout struct {
	nodefs.Node
	oe *stargz.TOCEntry
}

func (w *whiteout) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) fuse.Status {
	return entryToWhAttr(w.oe, out)
}

// inodeOfEnt calculates the inode number which is one-to-one conresspondence
// with the TOCEntry insntance.
func inodeOfEnt(e *stargz.TOCEntry) uint64 {
	return uint64(uintptr(unsafe.Pointer(e)))
}

// entryToAttr converts stargz's TOCEntry to go-fuse's Attr.
func entryToAttr(e *stargz.TOCEntry, out *fuse.Attr) fuse.Status {
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

// entryToWhAttr converts stargz's TOCEntry to go-fuse's Attr of whiteouts.
func entryToWhAttr(e *stargz.TOCEntry, out *fuse.Attr) fuse.Status {
	fi := e.Stat()
	out.Ino = inodeOfEnt(e)
	out.Size = 0
	out.Blksize = blockSize
	out.Blocks = 0
	out.Mtime = uint64(fi.ModTime().Unix())
	out.Mtimensec = uint32(fi.ModTime().UnixNano())
	out.Mode = syscall.S_IFCHR
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	out.Rdev = uint32(unix.Mkdev(0, 0))
	out.Nlink = 1
	out.Padding = 0 // TODO

	return fuse.OK
}

// fileModeToSystemMode converts os.FileMode to system's native bitmap.
func fileModeToSystemMode(m os.FileMode) uint32 {
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
