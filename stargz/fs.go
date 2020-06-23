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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const (
	blockSize                 = 4096
	memoryCacheType           = "memory"
	whiteoutPrefix            = ".wh."
	whiteoutOpaqueDir         = whiteoutPrefix + whiteoutPrefix + ".opq"
	opaqueXattr               = "trusted.overlay.opaque"
	opaqueXattrValue          = "y"
	stateDirName              = ".stargz-snapshotter"
	defaultResolveResultEntry = 100
	defaultPrefetchTimeoutSec = 10
	statFileMode              = syscall.S_IFREG | 0400 // -r--------
	stateDirMode              = syscall.S_IFDIR | 0500 // dr-x------

	// targetRefLabelCRI is a label which contains image reference passed from CRI plugin
	targetRefLabelCRI = "containerd.io/snapshot/cri.image-ref"
	// targetDigestLabelCRI is a label which contains layer digest passed from CRI plugin
	targetDigestLabelCRI = "containerd.io/snapshot/cri.layer-digest"
	// targetImageLayersLabel is a label which contains layer digests contained in
	// the target image and is passed from CRI plugin.
	targetImageLayersLabel = "containerd.io/snapshot/cri.image-layers"

	// PrefetchLandmark is a file entry which indicates the end position of
	// prefetch in the stargz file.
	PrefetchLandmark = ".prefetch.landmark"

	// NoPrefetchLandmark is a file entry which indicates that no prefetch should
	// occur in the stargz file.
	NoPrefetchLandmark = ".no.prefetch.landmark"
)

type Config struct {
	remote.ResolverConfig             `toml:"resolver"`
	remote.BlobConfig                 `toml:"blob"`
	keychain.KubeconfigKeychainConfig `toml:"kubeconfig_keychain"`
	cache.DirectoryCacheConfig        `toml:"directory_cache"`
	HTTPCacheType                     string `toml:"http_cache_type"`
	FSCacheType                       string `toml:"filesystem_cache_type"`
	ResolveResultEntry                int    `toml:"resolve_result_entry"`
	PrefetchSize                      int64  `toml:"prefetch_size"`
	PrefetchTimeoutSec                int64  `toml:"prefetch_timeout_sec"`
	NoPrefetch                        bool   `toml:"noprefetch"`
	Debug                             bool   `toml:"debug"`
}

func NewFilesystem(ctx context.Context, root string, config *Config) (_ snbase.FileSystem, err error) {
	var httpCache cache.BlobCache
	if config.HTTPCacheType == memoryCacheType {
		httpCache = cache.NewMemoryCache()
	} else {
		if httpCache, err = cache.NewDirectoryCache(
			filepath.Join(root, "http"),
			config.DirectoryCacheConfig,
		); err != nil {
			return nil, errors.Wrap(err, "failed to prepare HTTP cache")
		}
	}
	var fsCache cache.BlobCache
	if config.FSCacheType == memoryCacheType {
		fsCache = cache.NewMemoryCache()
	} else {
		if fsCache, err = cache.NewDirectoryCache(
			filepath.Join(root, "fscache"),
			config.DirectoryCacheConfig,
		); err != nil {
			return nil, errors.Wrap(err, "failed to prepare filesystem cache")
		}
	}
	keychain := authn.NewMultiKeychain(
		authn.DefaultKeychain,
		keychain.NewKubeconfigKeychain(ctx, config.KubeconfigKeychainConfig),
	)
	resolveResultEntry := config.ResolveResultEntry
	if resolveResultEntry == 0 {
		resolveResultEntry = defaultResolveResultEntry
	}
	prefetchTimeout := time.Duration(config.PrefetchTimeoutSec) * time.Second
	if prefetchTimeout == 0 {
		prefetchTimeout = defaultPrefetchTimeoutSec * time.Second
	}
	return &filesystem{
		resolver:              remote.NewResolver(keychain, config.ResolverConfig),
		blobConfig:            config.BlobConfig,
		httpCache:             httpCache,
		fsCache:               fsCache,
		prefetchSize:          config.PrefetchSize,
		prefetchTimeout:       prefetchTimeout,
		noprefetch:            config.NoPrefetch,
		debug:                 config.Debug,
		layer:                 make(map[string]*layer),
		resolveResult:         lru.New(resolveResultEntry),
		backgroundTaskManager: task.NewBackgroundTaskManager(2, 5*time.Second),
	}, nil
}

type filesystem struct {
	resolver              *remote.Resolver
	blobConfig            remote.BlobConfig
	httpCache             cache.BlobCache
	fsCache               cache.BlobCache
	prefetchSize          int64
	prefetchTimeout       time.Duration
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
		if c, ok := fs.resolveResult.Get(key); ok {
			if c.(*resolveResult).isInProgress() {
				rr = c.(*resolveResult) // resolving in progress
			} else if _, err := c.(*resolveResult).get(); err == nil {
				rr = c.(*resolveResult) // hit successfully resolved cache
			}
		}
		if rr == nil { // missed cache
			rr = fs.resolve(ctx, ref, dgst)
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
	l, err := resolved.waitAndGet(30 * time.Second) // get layer with timeout
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
		l.doPrefetch()
		go func() {
			defer l.donePrefetch()
			fs.backgroundTaskManager.DoPrioritizedTask()
			defer fs.backgroundTaskManager.DonePrioritizedTask()
			tr, err := fetchTr()
			if err != nil {
				logCtx.WithError(err).Debug("failed to prepare transport for prefetch")
				return
			}
			if err := l.prefetch(prefetchSize, remote.WithRoundTripper(tr)); err != nil {
				logCtx.WithError(err).Debug("failed to prefetched layer")
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
				retN, retErr = l.blob.ReadAt(
					p,
					offset,
					remote.WithContext(ctx),              // Make cancellable
					remote.WithRoundTripper(tr),          // Use dedicated Transport
					remote.WithCacheOpts(cache.Direct()), // Do not pollute mem cache
				)
			}, 120*time.Second)
			return
		}), 0, l.blob.Size())
		if err := l.reader.CacheTarGzWithReader(br, cache.Direct()); err != nil {
			logCtx.WithError(err).Debug("failed to fetch whole layer")
			return
		}
		logCtx.Debug("completed to fetch all layer data in background")
	}()

	// Mounting stargz
	// TODO: bind mount the state directory as a read-only fs on snapshotter's side
	timeSec := time.Second
	rawFS := fusefs.NewNodeFS(&node{
		fs:    fs,
		layer: l.reader,
		e:     l.root,
		s:     newState(digest, l.blob),
		root:  mountpoint,
	}, &fusefs.Options{
		AttrTimeout:     &timeSec,
		EntryTimeout:    &timeSec,
		NullPermissions: true,
	})
	server, err := fuse.NewServer(rawFS, mountpoint, &fuse.MountOptions{
		AllowOther: true,             // allow users other than root&mounter to access fs
		FsName:     "stargz",         // name this filesystem as "stargz"
		Options:    []string{"suid"}, // allow setuid inside container
		Debug:      fs.debug,
	})
	if err != nil {
		logCtx.WithError(err).Debug("failed to make filesystem server")
		return err
	}

	go func() {
		server.Serve()

		// Cleanup on unmount
		// TODO: prevent race.
		fs.layerMu.Lock()
		delete(fs.layer, mountpoint)
		fs.layerMu.Unlock()
	}()

	return server.WaitMount()
}

func (fs *filesystem) resolve(ctx context.Context, ref, digest string) *resolveResult {
	return newResolveResult(func() (*layer, error) {
		log.G(ctx).Debugf("resolving (%q, %q)", ref, digest)
		defer log.G(ctx).Debugf("resolved (%q, %q)", ref, digest)

		// Resolve the reference and digest
		blob, err := fs.resolver.Resolve(ref, digest, fs.httpCache, fs.blobConfig)
		if err != nil {
			return nil, errors.Wrap(err, "failed to resolve the reference")
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
			return nil, errors.Wrap(err, "failed to read layer")
		}

		return newLayer(blob, gr, root, fs.prefetchTimeout), nil
	})
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
	if err := l.waitForPrefetchCompletion(); err != nil {
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

func newResolveResult(init func() (*layer, error)) *resolveResult {
	rr := &resolveResult{
		progress: newWaiter(),
	}
	rr.progress.start()

	go func() {
		rr.resultMu.Lock()
		rr.layer, rr.err = init()
		rr.resultMu.Unlock()
		rr.progress.done()
	}()

	return rr
}

type resolveResult struct {
	layer    *layer
	err      error
	resultMu sync.Mutex
	progress *waiter
}

func (rr *resolveResult) waitAndGet(timeout time.Duration) (*layer, error) {
	if err := rr.progress.wait(timeout); err != nil {
		return nil, err
	}
	return rr.get()
}

func (rr *resolveResult) get() (*layer, error) {
	rr.resultMu.Lock()
	defer rr.resultMu.Unlock()
	if rr.layer == nil && rr.err == nil {
		return nil, fmt.Errorf("failed to get result")
	}
	return rr.layer, rr.err
}

func (rr *resolveResult) isInProgress() bool {
	return rr.progress.isInProgress()
}

func newLayer(blob remote.Blob, r reader.Reader, root *stargz.TOCEntry, prefetchTimeout time.Duration) *layer {
	return &layer{
		blob:            blob,
		reader:          r,
		root:            root,
		prefetchWaiter:  newWaiter(),
		prefetchTimeout: prefetchTimeout,
	}
}

type layer struct {
	blob            remote.Blob
	reader          reader.Reader
	root            *stargz.TOCEntry
	prefetchWaiter  *waiter
	prefetchTimeout time.Duration
}

func (l *layer) doPrefetch() {
	l.prefetchWaiter.start()
}

func (l *layer) donePrefetch() {
	l.prefetchWaiter.done()
}

func (l *layer) prefetch(prefetchSize int64, opts ...remote.Option) error {
	if _, ok := l.reader.Lookup(NoPrefetchLandmark); ok {
		// do not prefetch this layer
		return nil
	} else if e, ok := l.reader.Lookup(PrefetchLandmark); ok {
		// override the prefetch size with optimized value
		prefetchSize = e.Offset
	} else if prefetchSize > l.blob.Size() {
		// adjust prefetch size not to exceed the whole layer size
		prefetchSize = l.blob.Size()
	}

	if err := l.blob.Cache(0, prefetchSize, opts...); err != nil {
		return errors.Wrap(err, "failed to prefetch layer")
	}
	pr := io.NewSectionReader(readerAtFunc(func(p []byte, off int64) (int, error) {
		return l.blob.ReadAt(p, off, opts...)
	}), 0, prefetchSize)
	err := l.reader.CacheTarGzWithReader(pr)
	if err != nil && errors.Cause(err) != io.EOF && errors.Cause(err) != io.ErrUnexpectedEOF {
		return errors.Wrap(err, "failed to cache prefetched layer")
	}

	return nil
}

func (l *layer) waitForPrefetchCompletion() error {
	return l.prefetchWaiter.wait(l.prefetchTimeout)
}

func newWaiter() *waiter {
	return &waiter{
		completionCond: sync.NewCond(&sync.Mutex{}),
	}
}

type waiter struct {
	inProgress     bool
	inProgressMu   sync.Mutex
	completionCond *sync.Cond
}

func (w *waiter) start() {
	w.inProgressMu.Lock()
	w.inProgress = true
	w.inProgressMu.Unlock()
}

func (w *waiter) done() {
	w.inProgressMu.Lock()
	w.inProgress = false
	w.inProgressMu.Unlock()
	w.completionCond.Broadcast()
}

func (w *waiter) isInProgress() bool {
	w.inProgressMu.Lock()
	defer w.inProgressMu.Unlock()
	return w.inProgress
}

func (w *waiter) wait(timeout time.Duration) error {
	wait := func() <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			w.inProgressMu.Lock()
			inProgress := w.inProgress
			w.inProgressMu.Unlock()

			w.completionCond.L.Lock()
			if inProgress {
				w.completionCond.Wait()
			}
			w.completionCond.L.Unlock()
			ch <- struct{}{}
		}()
		return ch
	}
	select {
	case <-time.After(timeout):
		w.inProgressMu.Lock()
		w.inProgress = false
		w.inProgressMu.Unlock()
		w.completionCond.Broadcast()
		return fmt.Errorf("timeout(%v)", timeout)
	case <-wait():
		return nil
	}
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, offset int64) (int, error) { return f(p, offset) }

type fileReader interface {
	OpenFile(name string) (io.ReaderAt, error)
}

// node is a filesystem inode abstraction.
type node struct {
	fusefs.Inode
	fs     *filesystem
	layer  fileReader
	e      *stargz.TOCEntry
	s      *state
	root   string
	opaque bool // true if this node is an overlayfs opaque directory
}

var _ = (fusefs.InodeEmbedder)((*node)(nil))

var _ = (fusefs.NodeReaddirer)((*node)(nil))

func (n *node) Readdir(ctx context.Context) (fusefs.DirStream, syscall.Errno) {
	var ents []fuse.DirEntry
	whiteouts := map[string]*stargz.TOCEntry{}
	normalEnts := map[string]bool{}
	n.e.ForeachChild(func(baseName string, ent *stargz.TOCEntry) bool {

		// We don't want to show prefetch landmarks in "/".
		if n.e.Name == "" && (baseName == PrefetchLandmark || baseName == NoPrefetchLandmark) {
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

	return fusefs.NewListDirStream(ents), 0
}

var _ = (fusefs.NodeLookuper)((*node)(nil))

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	// We don't want to show prefetch landmarks in "/".
	if n.e.Name == "" && (name == PrefetchLandmark || name == NoPrefetchLandmark) {
		return nil, syscall.ENOENT
	}

	// We don't want to show whiteouts.
	if strings.HasPrefix(name, whiteoutPrefix) {
		return nil, syscall.ENOENT
	}

	// state directory
	if n.e.Name == "" && name == stateDirName {
		return n.NewInode(ctx, n.s, stateToAttr(n.s, &out.Attr)), 0
	}

	// lookup stargz TOCEntry
	ce, ok := n.e.LookupChild(name)
	if !ok {
		// If the entry exists as a whiteout, show an overlayfs-styled whiteout node.
		if wh, ok := n.e.LookupChild(fmt.Sprintf("%s%s", whiteoutPrefix, name)); ok {
			return n.NewInode(ctx, &whiteout{
				e: wh,
			}, entryToWhAttr(wh, &out.Attr)), 0
		}
		return nil, syscall.ENOENT
	}
	var opaque bool
	if _, ok := ce.LookupChild(whiteoutOpaqueDir); ok {
		// This entry is an opaque directory so make it recognizable for overlayfs.
		opaque = true
	}

	return n.NewInode(ctx, &node{
		fs:     n.fs,
		layer:  n.layer,
		e:      ce,
		s:      n.s,
		root:   n.root,
		opaque: opaque,
	}, entryToAttr(ce, &out.Attr)), 0
}

var _ = (fusefs.NodeOpener)((*node)(nil))

func (n *node) Open(ctx context.Context, flags uint32) (fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	ra, err := n.layer.OpenFile(n.e.Name)
	if err != nil {
		n.s.report(fmt.Errorf("failed to open node: %v", err))
		return nil, 0, syscall.EIO
	}
	return &file{
		n:  n,
		e:  n.e,
		ra: ra,
	}, 0, 0
}

var _ = (fusefs.NodeGetattrer)((*node)(nil))

func (n *node) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	entryToAttr(n.e, &out.Attr)
	return 0
}

var _ = (fusefs.NodeGetxattrer)((*node)(nil))

func (n *node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if attr == opaqueXattr && n.opaque {
		// This node is an opaque directory so give overlayfs-compliant indicator.
		if len(dest) < len(opaqueXattrValue) {
			return uint32(len(opaqueXattrValue)), syscall.ERANGE
		}
		return uint32(copy(dest, opaqueXattrValue)), 0
	}
	if v, ok := n.e.Xattrs[attr]; ok {
		if len(dest) < len(v) {
			return uint32(len(v)), syscall.ERANGE
		}
		return uint32(copy(dest, v)), 0
	}
	return 0, syscall.ENODATA
}

var _ = (fusefs.NodeListxattrer)((*node)(nil))

func (n *node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	var attrs []byte
	if n.opaque {
		// This node is an opaque directory so add overlayfs-compliant indicator.
		attrs = append(attrs, []byte(opaqueXattr+"\x00")...)
	}
	for k := range n.e.Xattrs {
		attrs = append(attrs, []byte(k+"\x00")...)
	}
	if len(dest) < len(attrs) {
		return uint32(len(attrs)), syscall.ERANGE
	}
	return uint32(copy(dest, attrs)), 0
}

var _ = (fusefs.NodeReadlinker)((*node)(nil))

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return []byte(n.e.LinkName), 0
}

var _ = (fusefs.NodeStatfser)((*node)(nil))

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	defaultStatfs(out)
	return 0
}

// file is a file abstraction which implements file handle in go-fuse.
type file struct {
	n  *node
	e  *stargz.TOCEntry
	ra io.ReaderAt
}

var _ = (fusefs.FileReader)((*file)(nil))

func (f *file) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.ra.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		f.n.s.report(fmt.Errorf("failed to read node: %v", err))
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

var _ = (fusefs.FileGetattrer)((*file)(nil))

func (f *file) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	entryToAttr(f.e, &out.Attr)
	return 0
}

// whiteout is a whiteout abstraction compliant to overlayfs.
type whiteout struct {
	fusefs.Inode
	e *stargz.TOCEntry
}

var _ = (fusefs.NodeGetattrer)((*whiteout)(nil))

func (w *whiteout) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	entryToWhAttr(w.e, &out.Attr)
	return 0
}

var _ = (fusefs.NodeStatfser)((*whiteout)(nil))

func (w *whiteout) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	defaultStatfs(out)
	return 0
}

// newState provides new state directory node.
// It creates statFile at the same time to give it stable inode number.
func newState(digest string, blob remote.Blob) *state {
	return &state{
		statFile: &statFile{
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
	fusefs.Inode
	statFile *statFile
}

var _ = (fusefs.NodeReaddirer)((*state)(nil))

func (s *state) Readdir(ctx context.Context) (fusefs.DirStream, syscall.Errno) {
	return fusefs.NewListDirStream([]fuse.DirEntry{
		{
			Mode: statFileMode,
			Name: s.statFile.name,
			Ino:  inodeOfStatFile(s.statFile),
		},
	}), 0
}

var _ = (fusefs.NodeLookuper)((*state)(nil))

func (s *state) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	if name != s.statFile.name {
		return nil, syscall.ENOENT
	}
	attr, errno := s.statFile.attr(&out.Attr)
	if errno != 0 {
		return nil, errno
	}
	return s.NewInode(ctx, s.statFile, attr), 0
}

var _ = (fusefs.NodeGetattrer)((*state)(nil))

func (s *state) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	stateToAttr(s, &out.Attr)
	return 0
}

var _ = (fusefs.NodeStatfser)((*state)(nil))

func (s *state) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	defaultStatfs(out)
	return 0
}

func (s *state) report(err error) {
	s.statFile.report(err)
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
// This file has mode "-r-------- root root".
type statFile struct {
	fusefs.Inode
	name     string
	blob     remote.Blob
	statJSON statJSON
	mu       sync.Mutex
}

var _ = (fusefs.NodeOpener)((*statFile)(nil))

func (sf *statFile) Open(ctx context.Context, flags uint32) (fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	return nil, 0, 0
}

var _ = (fusefs.NodeReader)((*statFile)(nil))

func (sf *statFile) Read(ctx context.Context, f fusefs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	st, err := sf.updateStatUnlocked()
	if err != nil {
		return nil, syscall.EIO
	}
	n, err := bytes.NewReader(st).ReadAt(dest, off)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

var _ = (fusefs.NodeGetattrer)((*statFile)(nil))

func (sf *statFile) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	_, errno := sf.attr(&out.Attr)
	return errno
}

var _ = (fusefs.NodeStatfser)((*statFile)(nil))

func (sf *statFile) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	defaultStatfs(out)
	return 0
}

func (sf *statFile) report(err error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	sf.statJSON.Error = err.Error()
}

func (sf *statFile) attr(out *fuse.Attr) (fusefs.StableAttr, syscall.Errno) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	st, err := sf.updateStatUnlocked()
	if err != nil {
		return fusefs.StableAttr{}, syscall.EIO
	}

	return statFileToAttr(sf, uint64(len(st)), out), 0
}

func (sf *statFile) updateStatUnlocked() ([]byte, error) {
	sf.statJSON.FetchedSize = sf.blob.FetchedSize()
	sf.statJSON.FetchedPercent = float64(sf.statJSON.FetchedSize) / float64(sf.statJSON.Size) * 100.0
	j, err := json.Marshal(&sf.statJSON)
	if err != nil {
		return nil, err
	}
	j = append(j, []byte("\n")...)
	return j, nil
}

// inodeOfEnt calculates the inode number which is one-to-one conresspondence
// with the TOCEntry insntance.
func inodeOfEnt(e *stargz.TOCEntry) uint64 {
	return uint64(uintptr(unsafe.Pointer(e)))
}

// entryToAttr converts stargz's TOCEntry to go-fuse's Attr.
func entryToAttr(e *stargz.TOCEntry, out *fuse.Attr) fusefs.StableAttr {
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

	return fusefs.StableAttr{
		Mode: out.Mode,
		Ino:  out.Ino,
		// NOTE: The inode number is unique throughout the lifettime of
		// this filesystem so we don't consider about generation at this
		// moment.
	}
}

// entryToWhAttr converts stargz's TOCEntry to go-fuse's Attr of whiteouts.
func entryToWhAttr(e *stargz.TOCEntry, out *fuse.Attr) fusefs.StableAttr {
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

	return fusefs.StableAttr{
		Mode: out.Mode,
		Ino:  out.Ino,
		// NOTE: The inode number is unique throughout the lifettime of
		// this filesystem so we don't consider about generation at this
		// moment.
	}
}

// inodeOfState calculates the inode number which is one-to-one conresspondence
// with the state directory insntance which was created on mount.
func inodeOfState(s *state) uint64 {
	return uint64(uintptr(unsafe.Pointer(s)))
}

// stateToAttr converts state directory to go-fuse's Attr.
func stateToAttr(s *state, out *fuse.Attr) fusefs.StableAttr {
	out.Ino = inodeOfState(s)
	out.Size = 0
	out.Blksize = blockSize
	out.Blocks = 0
	out.Nlink = 1

	// root can read and open it (dr-x------ root root).
	out.Mode = stateDirMode
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}

	// dummy
	out.Mtime = 0
	out.Mtimensec = 0
	out.Rdev = 0
	out.Padding = 0

	return fusefs.StableAttr{
		Mode: out.Mode,
		Ino:  out.Ino,
		// NOTE: The inode number is unique throughout the lifettime of
		// this filesystem so we don't consider about generation at this
		// moment.
	}
}

// inodeOfStatFile calculates the inode number which is one-to-one conresspondence
// with the stat file insntance which was created on mount.
func inodeOfStatFile(s *statFile) uint64 {
	return uint64(uintptr(unsafe.Pointer(s)))
}

// statFileToAttr converts stat file to go-fuse's Attr.
func statFileToAttr(sf *statFile, size uint64, out *fuse.Attr) fusefs.StableAttr {
	out.Ino = inodeOfStatFile(sf)
	out.Size = size
	out.Blksize = blockSize
	out.Blocks = out.Size / uint64(out.Blksize)
	out.Nlink = 1

	// Root can read it ("-r-------- root root").
	out.Mode = statFileMode
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}

	// dummy
	out.Mtime = 0
	out.Mtimensec = 0
	out.Rdev = 0
	out.Padding = 0

	return fusefs.StableAttr{
		Mode: out.Mode,
		Ino:  out.Ino,
		// NOTE: The inode number is unique throughout the lifettime of
		// this filesystem so we don't consider about generation at this
		// moment.
	}
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

func defaultStatfs(stat *fuse.StatfsOut) {

	// http://man7.org/linux/man-pages/man2/statfs.2.html
	stat.Blocks = 0 // dummy
	stat.Bfree = 0
	stat.Bavail = 0
	stat.Files = 0 // dummy
	stat.Ffree = 0
	stat.Bsize = blockSize
	stat.NameLen = 1<<32 - 1
	stat.Frsize = blockSize
	stat.Padding = 0
	stat.Spare = [6]uint32{}
}
