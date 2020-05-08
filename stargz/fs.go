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

package stargz

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/cache"
	snbase "github.com/containerd/stargz-snapshotter/snapshot"
	"github.com/containerd/stargz-snapshotter/stargz/handler"
	"github.com/containerd/stargz-snapshotter/stargz/keychain"
	"github.com/containerd/stargz-snapshotter/stargz/reader"
	"github.com/containerd/stargz-snapshotter/stargz/remote"
	"github.com/containerd/stargz-snapshotter/task"
	"github.com/golang/groupcache/lru"
	"github.com/google/crfs/stargz"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const (
	blockSize                 = 512
	memoryCacheType           = "memory"
	whiteoutPrefix            = ".wh."
	whiteoutOpaqueDir         = whiteoutPrefix + whiteoutPrefix + ".opq"
	opaqueXattr               = "trusted.overlay.opaque"
	opaqueXattrValue          = "y"
	stateDirName              = ".stargz-snapshotter"
	defaultLRUCacheEntry      = 5000
	defaultResolveResultEntry = 3000

	// targetRefLabelCRI is a label which contains image reference passed from CRI plugin
	targetRefLabelCRI = "containerd.io/snapshot/cri.image-ref"
	// targetDigestLabelCRI is a label which contains layer digest passed from CRI plugin
	targetDigestLabelCRI = "containerd.io/snapshot/cri.layer-digest"
	// targetImageLayersLabel is a label which contains layer digests contained in
	// the target image and is passed from CRI plugin.
	targetImageLayersLabel = "containerd.io/snapshot/cri.image-layers"
)

type Config struct {
	remote.ResolverConfig             `toml:"resolver"`
	remote.BlobConfig                 `toml:"blob"`
	keychain.KubeconfigKeychainConfig `toml:"kubeconfig_keychain"`
	HTTPCacheType                     string `toml:"http_cache_type"`
	FSCacheType                       string `toml:"filesystem_cache_type"`
	LRUCacheEntry                     int    `toml:"lru_max_entry"`
	ResolveResultEntry                int    `toml:"resolve_result_entry"`
	PrefetchSize                      int64  `toml:"prefetch_size"`
	NoPrefetch                        bool   `toml:"noprefetch"`
	Debug                             bool   `toml:"debug"`
}

func NewFilesystem(ctx context.Context, root string, config *Config) (snbase.FileSystem, error) {
	maxEntry := config.LRUCacheEntry
	if maxEntry == 0 {
		maxEntry = defaultLRUCacheEntry
	}
	httpCache, err := getCache(config.HTTPCacheType, filepath.Join(root, "httpcache"), maxEntry)
	if err != nil {
		return nil, err
	}
	fsCache, err := getCache(config.FSCacheType, filepath.Join(root, "fscache"), maxEntry)
	if err != nil {
		return nil, err
	}
	keychain := authn.NewMultiKeychain(
		authn.DefaultKeychain,
		keychain.NewKubeconfigKeychain(ctx, config.KubeconfigKeychainConfig),
	)
	resolveResultEntry := config.ResolveResultEntry
	if resolveResultEntry == 0 {
		resolveResultEntry = defaultResolveResultEntry
	}
	return &filesystem{
		resolver:              remote.NewResolver(keychain, config.ResolverConfig),
		blobConfig:            config.BlobConfig,
		httpCache:             httpCache,
		fsCache:               fsCache,
		prefetchSize:          config.PrefetchSize,
		noprefetch:            config.NoPrefetch,
		debug:                 config.Debug,
		layer:                 make(map[string]*layer),
		resolveResult:         lru.New(resolveResultEntry),
		backgroundTaskManager: task.NewBackgroundTaskManager(2, 5*time.Second),
	}, nil
}

// getCache gets a cache corresponding to specified type.
func getCache(ctype, dir string, maxEntry int) (cache.BlobCache, error) {
	if ctype == memoryCacheType {
		return cache.NewMemoryCache(), nil
	}
	return cache.NewDirectoryCache(dir, maxEntry)
}

type filesystem struct {
	resolver              *remote.Resolver
	blobConfig            remote.BlobConfig
	httpCache             cache.BlobCache
	fsCache               cache.BlobCache
	prefetchSize          int64
	noprefetch            bool
	debug                 bool
	layer                 map[string]*layer
	layerMu               sync.Mutex
	resolveResult         *lru.Cache
	resolveResultMu       sync.Mutex
	backgroundTaskManager *task.BackgroundTaskManager
}

func (fs *filesystem) Mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	// This is a prioritized task and all background tasks will be stopped
	// execution so this can avoid being disturbed for NW traffic by background
	// tasks.
	fs.backgroundTaskManager.DoPrioritizedTask()
	defer fs.backgroundTaskManager.DonePrioritizedTask()
	logCtx := log.G(ctx).WithField("mountpoint", mountpoint)

	// Get basic information of this layer.
	ref, digest, layers, prefetchSize, err := fs.parseLabels(labels)
	if err != nil {
		logCtx.WithError(err).Debug("failed to get necessary information from labels")
		return err
	}
	logCtx = logCtx.WithField("ref", ref).WithField("digest", digest)

	// Resolve the target layer and the all chained layers
	var (
		resolved *resolveResult
		target   = append([]string{digest}, layers...)
	)
	for _, dgst := range target {
		var (
			rr  *resolveResult
			key = fmt.Sprintf("%s/%s", ref, dgst)
		)
		fs.resolveResultMu.Lock()
		if cached, ok := fs.resolveResult.Get(key); ok && cached.(*resolveResult).err == nil {
			rr = cached.(*resolveResult) // hit cache
		} else {
			rr = fs.resolve(ctx, ref, dgst) // missed cache
			fs.resolveResult.Add(key, rr)
		}
		if dgst == digest {
			resolved = rr
		}
		fs.resolveResultMu.Unlock()
	}

	// Get the resolved layer
	if resolved == nil {
		logCtx.Debug("resolve result isn't registered")
		return fmt.Errorf("resolve result(%q,%q) isn't registered", ref, digest)
	}
	l, err := resolved.get(30 * time.Second) // get layer with timeout
	if err != nil {
		logCtx.WithError(err).Debug("failed to resolve layer")
		return errors.Wrapf(err, "failed to resolve layer(%q,%q)", ref, digest)
	}
	if err := fs.check(ctx, l); err != nil { // check the connectivity
		return err
	}

	// Register the mountpoint layer
	fs.layerMu.Lock()
	fs.layer[mountpoint] = l
	fs.layerMu.Unlock()

	// RoundTripper only used for pre-/background-fetch.
	// We use a separated transport because we don't want these fetching
	// functionalities to disturb other HTTP-related operations
	fetchTr := lazyTransport(func() (http.RoundTripper, error) {
		return l.blob.Authn(http.DefaultTransport.(*http.Transport).Clone())
	})

	// Prefetch this layer. We prefetch several layers in parallel. The first
	// Check() for this layer waits for the prefetch completion. We recreate
	// RoundTripper to avoid disturbing other NW-related operations.
	if !fs.noprefetch {
		go func() {
			pr := io.NewSectionReader(readerAtFunc(func(p []byte, offset int64) (int, error) {
				fs.backgroundTaskManager.DoPrioritizedTask()
				defer fs.backgroundTaskManager.DonePrioritizedTask()
				tr, err := fetchTr()
				if err != nil {
					logCtx.WithError(err).Debug("failed to prepare transport for prefetch")
					return 0, err
				}
				return l.blob.ReadAt(p, offset, remote.WithRoundTripper(tr))
			}), 0, l.blob.Size())
			if err := l.reader.PrefetchWithReader(pr, prefetchSize); err != nil {
				logCtx.WithError(err).Debug("failed to prefetch layer")
				return
			}
			logCtx.Debug("completed to prefetch")
		}()
	}

	// Fetch whole layer aggressively in background. We use background
	// reader for this so prioritized tasks(Mount, Check, etc...) can
	// interrupt the reading. This can avoid disturbing prioritized tasks
	// about NW traffic. We read layer with a buffer to reduce num of
	// requests to the registry.
	go func() {
		br := io.NewSectionReader(readerAtFunc(func(p []byte, offset int64) (retN int, retErr error) {
			fs.backgroundTaskManager.InvokeBackgroundTask(func(ctx context.Context) {
				tr, err := fetchTr()
				if err != nil {
					logCtx.WithError(err).Debug("failed to prepare transport for background fetch")
					retN, retErr = 0, err
					return
				}
				retN, retErr = l.blob.ReadAt(p, offset, remote.WithContext(ctx), remote.WithRoundTripper(tr))
			}, 120*time.Second)
			return
		}), 0, l.blob.Size())
		if err := l.reader.CacheTarGzWithReader(bufio.NewReaderSize(br, 1<<29)); err != nil {
			logCtx.WithError(err).Debug("failed to fetch whole layer")
			return
		}
		logCtx.Debug("completed to fetch all layer data in background")
	}()

	// Mounting stargz
	// TODO: bind mount the state directory as a read-only fs on snapshotter's side
	conn := nodefs.NewFileSystemConnector(&node{
		Node:  nodefs.NewDefaultNode(),
		fs:    fs,
		layer: l.reader,
		e:     l.root,
		s:     newState(digest, l.blob),
		root:  mountpoint,
	}, &nodefs.Options{
		NegativeTimeout: 0,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		Owner:           nil, // preserve owners.
	})
	server, err := fuse.NewServer(conn.RawFS(), mountpoint, &fuse.MountOptions{
		AllowOther: true,             // allow users other than root&mounter to access fs
		Options:    []string{"suid"}, // allow setuid inside container
	})
	if err != nil {
		logCtx.WithError(err).Debug("failed to make filesstem server")
		return err
	}

	server.SetDebug(fs.debug)
	go server.Serve()
	return server.WaitMount()
}

func (fs *filesystem) resolve(ctx context.Context, ref, digest string) *resolveResult {
	rr := &resolveResult{
		resolveCompletionCond: sync.NewCond(&sync.Mutex{}),
	}
	rr.doingResolve()

	go func() {
		defer rr.doneResolve()

		log.G(ctx).Debugf("resolving (%q, %q)", ref, digest)
		defer log.G(ctx).Debugf("resolved (%q, %q)", ref, digest)

		// Resolve the reference and digest
		blob, err := fs.resolver.Resolve(ref, digest, fs.httpCache, fs.blobConfig)
		if err != nil {
			rr.err = errors.Wrap(err, "failed to resolve the reference")
			return
		}

		// Get a reader for stargz archive.
		// Each file's read operation is a prioritized task and all background tasks
		// will be stopped during the execution so this can avoid being disturbed for
		// NW traffic by background tasks.
		sr := io.NewSectionReader(readerAtFunc(func(p []byte, offset int64) (n int, err error) {
			fs.backgroundTaskManager.DoPrioritizedTask()
			defer fs.backgroundTaskManager.DonePrioritizedTask()
			return blob.ReadAt(p, offset)
		}), 0, blob.Size())
		gr, root, err := reader.NewReader(sr, fs.fsCache)
		if err != nil {
			rr.err = errors.Wrap(err, "failed to read layer")
			return
		}

		rr.layer = &layer{
			blob:   blob,
			reader: gr,
			root:   root,
		}
	}()

	return rr
}

func (fs *filesystem) Check(ctx context.Context, mountpoint string) error {
	// This is a prioritized task and all background tasks will be stopped
	// execution so this can avoid being disturbed for NW traffic by background
	// tasks.
	fs.backgroundTaskManager.DoPrioritizedTask()
	defer fs.backgroundTaskManager.DonePrioritizedTask()

	logCtx := log.G(ctx).WithField("mountpoint", mountpoint)

	fs.layerMu.Lock()
	l := fs.layer[mountpoint]
	fs.layerMu.Unlock()
	if l == nil {
		logCtx.Debug("layer not registered")
		return fmt.Errorf("layer not registered")
	}

	// Wait for prefetch compeletion
	if err := l.reader.WaitForPrefetchCompletion(10 * time.Second); err != nil {
		logCtx.WithError(err).Warn("failed to sync with prefetch completion")
	}

	// Check the blob connectivity and refresh the connection if possible
	if err := fs.check(ctx, l); err != nil {
		logCtx.WithError(err).Warn("check failed")
		return err
	}

	return nil
}

func (fs *filesystem) check(ctx context.Context, l *layer) error {
	logCtx := log.G(ctx)

	if err := l.blob.Check(); err != nil {
		// Check failed. Try to refresh the connection
		logCtx.WithError(err).Warn("failed to connect to blob; refreshing...")
		for retry := 0; retry < 3; retry++ {
			if iErr := fs.resolver.Refresh(l.blob); iErr != nil {
				logCtx.WithError(iErr).Warnf("failed to refresh connection(%d)", retry)
				err = errors.Wrapf(err, "error(%d): %v", retry, iErr)
				continue // retry
			}
			logCtx.Debug("Successfully refreshed connection")
			err = nil
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func (fs *filesystem) unregister(mountpoint string) {
	fs.layerMu.Lock()
	delete(fs.layer, mountpoint)
	fs.layerMu.Unlock()
}

func (fs *filesystem) parseLabels(labels map[string]string) (rRef, rDigest string, rLayers []string, rPrefetchSize int64, _ error) {

	// mandatory labels
	if ref, ok := labels[targetRefLabelCRI]; ok {
		rRef = ref
	} else if ref, ok := labels[handler.TargetRefLabel]; ok {
		rRef = ref
	} else {
		return "", "", nil, 0, fmt.Errorf("reference hasn't been passed")
	}
	if digest, ok := labels[targetDigestLabelCRI]; ok {
		rDigest = digest
	} else if digest, ok := labels[handler.TargetDigestLabel]; ok {
		rDigest = digest
	} else {
		return "", "", nil, 0, fmt.Errorf("digest hasn't been passed")
	}
	if l, ok := labels[targetImageLayersLabel]; ok {
		rLayers = strings.Split(l, ",")
	} else if l, ok := labels[handler.TargetImageLayersLabel]; ok {
		rLayers = strings.Split(l, ",")
	} else {
		return "", "", nil, 0, fmt.Errorf("image layers hasn't been passed")
	}

	// optional label
	rPrefetchSize = fs.prefetchSize
	if psStr, ok := labels[handler.TargetPrefetchSizeLabel]; ok {
		if ps, err := strconv.ParseInt(psStr, 10, 64); err == nil {
			rPrefetchSize = ps
		}
	}

	return
}

func lazyTransport(trFunc func() (http.RoundTripper, error)) func() (http.RoundTripper, error) {
	var (
		tr   http.RoundTripper
		trMu sync.Mutex
	)
	return func() (http.RoundTripper, error) {
		trMu.Lock()
		defer trMu.Unlock()
		if tr != nil {
			return tr, nil
		}
		gotTr, err := trFunc()
		if err != nil {
			return nil, err
		}
		tr = gotTr
		return tr, nil
	}
}

type layer struct {
	blob   remote.Blob
	reader reader.Reader
	root   *stargz.TOCEntry
}

type resolveResult struct {
	layer                 *layer
	err                   error
	resolveInProgress     bool
	resolveCompletionCond *sync.Cond
}

func (rr *resolveResult) doingResolve() {
	rr.resolveInProgress = true
}

func (rr *resolveResult) doneResolve() {
	rr.resolveInProgress = false
	rr.resolveCompletionCond.Broadcast()
}

func (rr *resolveResult) get(timeout time.Duration) (*layer, error) {
	waitUntilResolved := func() <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			if rr.resolveInProgress {
				rr.resolveCompletionCond.L.Lock()
				rr.resolveCompletionCond.Wait()
				rr.resolveCompletionCond.L.Unlock()
			}
			ch <- struct{}{}
		}()
		return ch
	}
	select {
	case <-time.After(timeout):
		rr.resolveInProgress = false
		rr.resolveCompletionCond.Broadcast()
		return nil, fmt.Errorf("timeout(%v)", timeout)
	case <-waitUntilResolved():
		return rr.layer, rr.err
	}
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, offset int64) (int, error) { return f(p, offset) }

type fileReader interface {
	OpenFile(name string) (io.ReaderAt, error)
}

// node is a filesystem inode abstraction which implements node in go-fuse.
type node struct {
	nodefs.Node
	fs     *filesystem
	layer  fileReader
	e      *stargz.TOCEntry
	s      *state
	root   string
	opaque bool // true if this node is an overlayfs opaque directory
}

func (n *node) OnUnmount() {
	n.fs.unregister(n.root)
}

func (n *node) OpenDir(context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	var ents []fuse.DirEntry
	whiteouts := map[string]*stargz.TOCEntry{}
	normalEnts := map[string]bool{}
	n.e.ForeachChild(func(baseName string, ent *stargz.TOCEntry) bool {

		// We don't want to show prefetch landmarks in "/".
		if n.e.Name == "" && (baseName == reader.PrefetchLandmark || baseName == reader.NoPrefetchLandmark) {
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
			Mode: modeOfEntry(ent),
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

	// We don't want to show prefetch landmarks in "/".
	if n.e.Name == "" && (name == reader.PrefetchLandmark || name == reader.NoPrefetchLandmark) {
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
		fs:     n.fs,
		layer:  n.layer,
		e:      ce,
		s:      n.s,
		root:   n.root,
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
	if mode<<shift&modeOfEntry(n.e) != 0 {
		return fuse.OK
	}

	return fuse.EPERM
}

func (n *node) Open(flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	ra, err := n.layer.OpenFile(n.e.Name)
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
	for k := range n.e.Xattrs {
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

// file is a file abstraction which implements file in go-fuse.
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
	if err != nil && err != io.EOF {
		f.n.s.report(fmt.Errorf("failed to read node: %v", err))
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(buf[:n]), fuse.OK
}

func (f *file) GetAttr(out *fuse.Attr) fuse.Status {
	return entryToAttr(f.e, out)
}

// whiteout is a whiteout abstraction compliant to overlayfs. This implements
// node in go-fuse.
type whiteout struct {
	nodefs.Node
	oe *stargz.TOCEntry
}

func (w *whiteout) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) fuse.Status {
	return entryToWhAttr(w.oe, out)
}

// newState provides new state directory node.
// It creates statFile at the same time to give it stable inode number.
func newState(digest string, blob remote.Blob) *state {
	return &state{
		Node: nodefs.NewDefaultNode(),
		statFile: &statFile{
			Node: nodefs.NewDefaultNode(),
			name: digest + ".json",
			statJSON: statJSON{
				Digest: digest,
				Size:   blob.Size(),
			},
			blob: blob,
		},
	}
}

// state is a directory which contain a "state file" of this layer aming to
// observability. This filesystem uses it to report something(e.g. error) to
// the clients(e.g. Kubernetes's livenessProbe).
// This directory has mode "dr-x------ root root".
type state struct {
	nodefs.Node
	statFile *statFile
}

func (s *state) report(err error) {
	s.statFile.report(err)
}

func (s *state) OpenDir(context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	return []fuse.DirEntry{
		{
			Mode: syscall.S_IFREG | s.statFile.mode(),
			Name: s.statFile.name,
			Ino:  s.statFile.ino(),
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

	if name != s.statFile.name {
		return nil, fuse.ENOENT
	}
	return s.Inode().NewChild(name, false, s.statFile), s.statFile.attr(out)
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

func (s *state) mode() uint32 {
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

type statJSON struct {
	Error  string `json:"error,omitempty"`
	Digest string `json:"digest"`
	// URL is excluded for potential security reason
	Size           int64   `json:"size"`
	FetchedSize    int64   `json:"fetchedSize"`
	FetchedPercent float64 `json:"fetchedPercent"` // Fetched / Size * 100.0
}

// statFile is a file which contain something to be reported from this layer.
// This filesystem uses statFile.report() to report something(e.g. error) to
// the clients(e.g. Kubernetes's livenessProbe).
// This directory has mode "-r-------- root root".
type statFile struct {
	nodefs.Node
	name     string
	blob     remote.Blob
	statJSON statJSON
	mu       sync.Mutex
}

func (e *statFile) report(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statJSON.Error = err.Error()
}

func (e *statFile) updateStatUnlocked() ([]byte, error) {
	e.statJSON.FetchedSize = e.blob.FetchedSize()
	e.statJSON.FetchedPercent = float64(e.statJSON.FetchedSize) / float64(e.statJSON.Size) * 100.0
	j, err := json.Marshal(&e.statJSON)
	if err != nil {
		return nil, err
	}
	j = append(j, []byte("\n")...)
	return j, nil
}

func (e *statFile) Access(mode uint32, context *fuse.Context) fuse.Status {
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

func (e *statFile) Open(flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	return nil, fuse.OK
}

func (e *statFile) Read(file nodefs.File, dest []byte, off int64, context *fuse.Context) (fuse.ReadResult, fuse.Status) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, err := e.updateStatUnlocked()
	if err != nil {
		return nil, fuse.EIO
	}
	n, err := bytes.NewReader(st).ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, fuse.EIO
	}
	return fuse.ReadResultData(dest[:n]), fuse.OK
}

func (e *statFile) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) fuse.Status {
	return e.attr(out)
}

func (e *statFile) StatFs() *fuse.StatfsOut {
	return defaultStatfs()
}

func (e *statFile) ino() uint64 {
	// calculates the inode number which is one-to-one conresspondence
	// with this state file node inscance.
	return uint64(uintptr(unsafe.Pointer(e)))
}

func (e *statFile) mode() uint32 {
	return 0400
}

func (e *statFile) attr(out *fuse.Attr) fuse.Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	st, err := e.updateStatUnlocked()
	if err != nil {
		return fuse.EIO
	}

	out.Ino = e.ino()
	out.Size = uint64(len(st))
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
	out.Ino = inodeOfEnt(e)
	out.Size = uint64(e.Size)
	out.Blksize = blockSize
	out.Blocks = out.Size / uint64(out.Blksize)
	if out.Size%uint64(out.Blksize) > 0 {
		out.Blocks++
	}
	out.Mtime = uint64(e.ModTime().Unix())
	out.Mtimensec = uint32(e.ModTime().UnixNano())
	out.Mode = modeOfEntry(e)
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

// modeOfEntry gets system's mode bits from TOCEntry
func modeOfEntry(e *stargz.TOCEntry) uint32 {
	// Permission bits
	res := uint32(e.Stat().Mode() & os.ModePerm)

	// File type bits
	switch e.Stat().Mode() & os.ModeType {
	case os.ModeDevice:
		res |= syscall.S_IFBLK
	case os.ModeDevice | os.ModeCharDevice:
		res |= syscall.S_IFCHR
	case os.ModeDir:
		res |= syscall.S_IFDIR
	case os.ModeNamedPipe:
		res |= syscall.S_IFIFO
	case os.ModeSymlink:
		res |= syscall.S_IFLNK
	case os.ModeSocket:
		res |= syscall.S_IFSOCK
	default: // regular file.
		res |= syscall.S_IFREG
	}

	// SUID, SGID, Sticky bits
	// Stargz package doesn't provide these bits so let's calculate them manually
	// here. TOCEntry.Mode is a copy of tar.Header.Mode so we can understand the
	// mode using that package.
	// See also:
	// - https://github.com/google/crfs/blob/71d77da419c90be7b05d12e59945ac7a8c94a543/stargz/stargz.go#L706
	hm := (&tar.Header{Mode: e.Mode}).FileInfo().Mode()
	if hm&os.ModeSetuid != 0 {
		res |= syscall.S_ISUID
	}
	if hm&os.ModeSetgid != 0 {
		res |= syscall.S_ISGID
	}
	if hm&os.ModeSticky != 0 {
		res |= syscall.S_ISVTX
	}

	return res
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
