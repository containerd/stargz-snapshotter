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
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	defaultCacheMaxEntry  = 1000
	directoryCacheType    = "directory"
	memoryCacheType       = "memory"
	whiteoutPrefix        = ".wh."
	whiteoutOpaqueDir     = whiteoutPrefix + whiteoutPrefix + ".opq"
	opaqueXattr           = "trusted.overlay.opaque"
	opaqueXattrValue      = "y"
	prefetchLandmark      = ".prefetch.landmark"
	stateDirName          = ".remote-snapshotter"
)

type Config struct {
	Insecure       []string `toml:"insecure"`
	CacheChunkSize int64    `toml:"cache_chunk_size"`
	PrefetchSize   int64    `toml:"prefetch_size"`
	NoPrefetch     bool     `toml:"noprefetch"`
	LRUCacheEntry  int      `toml:"lru_max_entry"`
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
func getCache(ctype, dir string, maxEntry int) (cache.BlobCache, error) {
	switch ctype {
	case directoryCacheType:
		return cache.NewDirectoryCache(dir, maxEntry)
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
		noprefetch:     config.NoPrefetch,
		transport:      http.DefaultTransport,
		url:            make(map[string]string),
	}
	if fs.cacheChunkSize == 0 {
		fs.cacheChunkSize = defaultCacheChunkSize
	}
	fs.httpCache, err = getCache(config.HTTPCacheType, filepath.Join(root, "httpcache"), config.LRUCacheEntry)
	if err != nil {
		return nil, err
	}
	fs.fsCache, err = getCache(config.FSCacheType, filepath.Join(root, "fscache"), config.LRUCacheEntry)
	if err != nil {
		return nil, err
	}

	return
}

type filesystem struct {
	insecure       []string
	cacheChunkSize int64
	prefetchSize   int64
	noprefetch     bool
	httpCache      cache.BlobCache
	fsCache        cache.BlobCache
	transport      http.RoundTripper
	url            map[string]string
	mu             sync.Mutex
}

func (fs *filesystem) Mount(ref, digest, mountpoint string) error {
	host, url, err := fs.parseReference(ref, digest)
	if err != nil {
		return fmt.Errorf("failed to parse the reference %q: %v", ref, err)
	}

	// authenticate to the registry using ~/.docker/config.json.
	url, tr, err := fs.resolve(host, url, ref)
	if err != nil {
		return fmt.Errorf("failed to resolve the reference %q: %v", ref, err)
	}
	fs.mu.Lock()
	fs.url[mountpoint] = url
	fs.mu.Unlock()

	// Get size information.
	size, err := fs.getSize(tr, url)
	if err != nil {
		return fmt.Errorf("failed to get layer size information from %q: %v", url, err)
	}

	// Construct filesystem from the remote stargz layer.
	sr := io.NewSectionReader(&urlReaderAt{
		url:       url,
		t:         tr,
		size:      size,
		chunkSize: fs.cacheChunkSize,
		cache:     fs.httpCache,
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
		cache:  fs.fsCache,
	}
	if !fs.noprefetch {
		// TODO: make sync/async switchable
		if _, err := gr.prefetch(sr, fs.prefetchSize); err != nil {
			return err
		}
	}

	// Mounting stargz
	// TODO: bind mount the state directory as a read-only fs on snapshotter's side
	conn := nodefs.NewFileSystemConnector(&node{
		Node: nodefs.NewDefaultNode(),
		gr:   gr,
		e:    root,
		s:    newState(fmt.Sprintf("%x", sha256.Sum256([]byte(mountpoint)))),
	}, &nodefs.Options{
		NegativeTimeout: 0,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		Owner:           nil, // preserve owners.
	})
	server, err := fuse.NewServer(conn.RawFS(), mountpoint, &fuse.MountOptions{})
	if err != nil {
		return fmt.Errorf("failed to make server: %v", err)
	}
	// server.SetDebug(true) // TODO: make configurable with /etc/containerd/config.toml
	go server.Serve()
	return server.WaitMount()
}

// TODO: snapshotter's side. maybe after metadata snapshotter's patch has been done?
func (fs *filesystem) Check(mountpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fs.mu.Lock()
	url := fs.url[mountpoint]
	fs.mu.Unlock()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to request to the registry of %q: %v", url, err)
	}
	req.WithContext(ctx)
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := fs.transport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code on %q: %v", fs.url, res.Status)
	}

	return nil
}

// parseReference parses reference and digest then makes an URL of the blob.
// "ref" must be formatted in <host>/<image>[:<tag>] (<tag> is optional).
// "diegest" is an OCI's sha256 digest and must be prefixed by "sha256:".
func (fs *filesystem) parseReference(ref, digest string) (host string, url string, err error) {
	refs := strings.Split(ref, "/")
	if len(refs) < 2 {
		err = fmt.Errorf("reference must contain names of host and image but got %q", ref)
		return
	}
	scheme, host, image := "https", refs[0], strings.Split(strings.Join(refs[1:], "/"), ":")[0]
	if fs.isInsecure(host) {
		scheme = "http"
	}
	return host, fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme, host, image, digest), nil
}

// isInsecure checks if the specified host is registerd as "insecure" registry
// in this filesystem. If so, this filesystem treat the host in a proper way
// e.g. using HTTP instead of HTTPS.
func (fs *filesystem) isInsecure(host string) bool {
	for _, i := range fs.insecure {
		if ok, _ := regexp.Match(i, []byte(host)); ok {
			return true
		}
	}

	return false
}

// resolve resolves specified url and ref by authenticating and dealing with
// redirection in a proper way. We use `~/.docker/config.json` for authn.
func (fs *filesystem) resolve(host, url, ref string) (string, http.RoundTripper, error) {
	var opts []name.Option
	if fs.isInsecure(host) {
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
	tr, err := transport.New(nameref.Context().Registry, auth, fs.transport, []string{nameref.Scope(transport.PullScope)})
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
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	if res, err := tr.RoundTrip(req); err == nil && res.StatusCode < 400 {
		defer func() {
			io.Copy(ioutil.Discard, res.Body)
			res.Body.Close()
		}()
		if redir := res.Header.Get("Location"); redir != "" && res.StatusCode/100 == 3 {
			url = redir
		}
	}

	return url, tr, nil
}

// getSize fetches the size info of the specified layer by requesting HEAD.
func (fs *filesystem) getSize(tr http.RoundTripper, url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	req.WithContext(ctx)
	req.Close = false
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
	s      *state
	opaque bool // true if this node is an overlayfs opaque directory
}

func (n *node) OpenDir(context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	var ents []fuse.DirEntry
	whiteouts := map[string]*stargz.TOCEntry{}
	normalEnts := map[string]bool{}
	n.e.ForeachChild(func(baseName string, ent *stargz.TOCEntry) bool {

		// We don't want to show prefetch landmark in "/".
		if n.e.Name == "" && baseName == prefetchLandmark {
			return true
		}

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
		if !normalEnts[w[len(whiteoutPrefix):]] {
			ents = append(ents, fuse.DirEntry{
				Mode: syscall.S_IFCHR,
				Name: w[len(whiteoutPrefix):],
				Ino:  inodeOfEnt(ent),
			})

		}
	}

	// Append state directory in "/".
	if n.e.Name == "" {
		ents = append(ents, fuse.DirEntry{
			Mode: syscall.S_IFDIR | n.s.mode(),
			Name: stateDirName,
			Ino:  n.s.ino(),
		})
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

	// We don't want to show prefetch landmark in "/".
	if n.e.Name == "" && name == prefetchLandmark {
		return nil, fuse.ENOENT
	}

	// We don't want to show whiteouts.
	if strings.HasPrefix(name, whiteoutPrefix) {
		return nil, fuse.ENOENT
	}

	// state directory
	if n.e.Name == "" && name == stateDirName {
		return n.Inode().NewChild(name, true, n.s), n.s.attr(out)
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
		s:      n.s,
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
		n.s.report(fmt.Errorf("failed to open node: %v", err))
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
	return defaultStatfs()
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
	n, err := f.ra.ReadAt(buf, off)
	if err != nil {
		f.n.s.report(fmt.Errorf("failed to read node: %v", err))
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(buf[:n]), fuse.OK
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

// newState provides new state directory node.
// It creates errFile at the same time to give it stable inode number.
func newState(id string) *state {
	return &state{
		Node: nodefs.NewDefaultNode(),
		id:   id,
		err: &errFile{
			Node: nodefs.NewDefaultNode(),
		},
	}
}

// state is a directory which contain a "state file" of this layer aming to
// observability. This filesystem uses it to report something(e.g. error) to
// the clients(e.g. Kubernetes's livenessProbe).
// This directory has mode "dr-x------ root root".
type state struct {
	nodefs.Node
	id  string
	err *errFile
}

func (s *state) report(err error) {
	s.err.report(err)
}

func (s *state) OpenDir(context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	return []fuse.DirEntry{
		fuse.DirEntry{
			Mode: syscall.S_IFREG | s.err.mode(),
			Name: s.id,
			Ino:  s.err.ino(),
		},
	}, fuse.OK
}

func (s *state) Lookup(out *fuse.Attr, name string, context *fuse.Context) (*nodefs.Inode, fuse.Status) {
	if c := s.Inode().GetChild(name); c != nil {
		if status := c.Node().GetAttr(out, nil, context); status != fuse.OK {
			return nil, status
		}
		return c, fuse.OK
	}

	if name != s.id {
		return nil, fuse.ENOENT
	}
	return s.Inode().NewChild(name, false, s.err), s.err.attr(out)
}

func (s *state) Access(mode uint32, context *fuse.Context) fuse.Status {
	if mode == 0 {
		// Requires nothing.
		return fuse.OK
	}
	if context.Owner.Uid == 0 && mode&s.mode()>>6 != 0 {
		// root can read and open it (dr-x------ root root).
		return fuse.OK
	}

	return fuse.EPERM

}
func (s *state) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) fuse.Status {
	return s.attr(out)
}

func (s *state) StatFs() *fuse.StatfsOut {
	return defaultStatfs()
}

func (s *state) ino() uint64 {
	// calculates the inode number which is one-to-one conresspondence
	// with this state directory node inscance.
	return uint64(uintptr(unsafe.Pointer(s)))
}

func (e *state) mode() uint32 {
	return 0500
}

func (s *state) attr(out *fuse.Attr) fuse.Status {
	out.Ino = s.ino()
	out.Size = 0
	out.Blksize = blockSize
	out.Blocks = 0
	out.Mode = syscall.S_IFDIR | s.mode()
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	out.Nlink = 1

	// dummy
	out.Mtime = 0
	out.Mtimensec = 0
	out.Rdev = 0
	out.Padding = 0

	return fuse.OK
}

// errFile is a file which contain something to be reported from this layer.
// This filesystem uses errFile.report() to report something(e.g. error) to
// the clients(e.g. Kubernetes's livenessProbe).
// This directory has mode "-r-------- root root".
type errFile struct {
	nodefs.Node
	err []byte
	mu  sync.Mutex
}

func (e *errFile) report(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = []byte(fmt.Sprintf("%v", err))
}

func (e *errFile) Access(mode uint32, context *fuse.Context) fuse.Status {
	if mode == 0 {
		// Requires nothing.
		return fuse.OK
	}
	if context.Owner.Uid == 0 && mode&e.mode()>>6 != 0 {
		// root can operate it.
		return fuse.OK
	}

	return fuse.EPERM
}

func (e *errFile) Open(flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	return nil, fuse.OK
}

func (e *errFile) Read(file nodefs.File, dest []byte, off int64, context *fuse.Context) (fuse.ReadResult, fuse.Status) {
	e.mu.Lock()
	defer e.mu.Unlock()

	n, err := bytes.NewReader(e.err).ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(dest[:n]), fuse.OK
}

func (e *errFile) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) fuse.Status {
	return e.attr(out)
}

func (e *errFile) StatFs() *fuse.StatfsOut {
	return defaultStatfs()
}

func (e *errFile) ino() uint64 {
	// calculates the inode number which is one-to-one conresspondence
	// with this state file node inscance.
	return uint64(uintptr(unsafe.Pointer(e)))
}

func (e *errFile) mode() uint32 {
	return 0400
}

func (e *errFile) attr(out *fuse.Attr) fuse.Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	out.Ino = e.ino()
	out.Size = uint64(len(e.err))
	out.Blksize = blockSize
	out.Blocks = out.Size / uint64(out.Blksize)
	out.Mode = syscall.S_IFREG | e.mode()
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	out.Nlink = 1

	// dummy
	out.Mtime = 0
	out.Mtimensec = 0
	out.Rdev = 0
	out.Padding = 0

	return fuse.OK
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

func defaultStatfs() *fuse.StatfsOut {
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
