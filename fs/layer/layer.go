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

package layer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/fs/reader"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	"github.com/containerd/stargz-snapshotter/task"
	"github.com/containerd/stargz-snapshotter/util/lrucache"
	"github.com/containerd/stargz-snapshotter/util/namedmutex"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	defaultResolveResultEntry = 30
	defaultMaxLRUCacheEntry   = 10
	defaultMaxCacheFds        = 10
	defaultPrefetchTimeoutSec = 10
	memoryCacheType           = "memory"
)

// Layer represents a layer.
type Layer interface {

	// Info returns the information of this layer.
	Info() Info

	// Root returns the root node of this layer.
	Root() *estargz.TOCEntry

	// Check checks if the layer is still connectable.
	Check() error

	// Refresh refreshes the layer connection.
	Refresh(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error

	// Verify verifies this layer using the passed TOC Digest.
	Verify(tocDigest digest.Digest) (err error)

	// SkipVerify skips verification for this layer.
	SkipVerify() (err error)

	// OpenFile opens a file.
	// Calling this function before calling Verify or SkipVerify will fail.
	OpenFile(name string) (io.ReaderAt, error)

	// Prefetch prefetches the specified size. If the layer is eStargz and contains landmark files,
	// the range indicated by these files is respected.
	// Calling this function before calling Verify or SkipVerify will fail.
	Prefetch(prefetchSize int64) error

	// WaitForPrefetchCompletion waits untils Prefetch completes.
	WaitForPrefetchCompletion() error

	// BackgroundFetch fetches the entire layer contents to the cache.
	// Fetching contents is done as a background task.
	// Calling this function before calling Verify or SkipVerify will fail.
	BackgroundFetch() error

	// Done releases the reference to this layer. The resources related to this layer will be
	// discarded sooner or later. Queries after calling this function won't be serviced.
	Done()
}

// Info is the current status of a layer.
type Info struct {
	Digest      digest.Digest
	Size        int64
	FetchedSize int64
}

// Resolver resolves the layer location and provieds the handler of that layer.
type Resolver struct {
	resolver              *remote.Resolver
	prefetchTimeout       time.Duration
	layerCache            *lrucache.Cache
	layerCacheMu          sync.Mutex
	blobCache             *lrucache.Cache
	blobCacheMu           sync.Mutex
	backgroundTaskManager *task.BackgroundTaskManager
	fsCache               func() (cache.BlobCache, error)
	resolveLock           *namedmutex.NamedMutex
}

// NewResolver returns a new layer resolver.
func NewResolver(root string, backgroundTaskManager *task.BackgroundTaskManager, cfg config.Config) *Resolver {
	resolveResultEntry := cfg.ResolveResultEntry
	if resolveResultEntry == 0 {
		resolveResultEntry = defaultResolveResultEntry
	}
	prefetchTimeout := time.Duration(cfg.PrefetchTimeoutSec) * time.Second
	if prefetchTimeout == 0 {
		prefetchTimeout = defaultPrefetchTimeoutSec * time.Second
	}

	// layerCache caches resolved layers for future use. This is useful in a use-case where
	// the filesystem resolves and caches all layers in an image (not only queried one) in parallel,
	// before they are actually queried.
	layerCache := lrucache.New(resolveResultEntry)
	layerCache.OnEvicted = func(key string, value interface{}) {
		if err := value.(*layer).close(); err != nil {
			logrus.WithField("key", key).WithError(err).Warnf("failed to clean up layer")
			return
		}
		logrus.WithField("key", key).Debugf("cleaned up layer")
	}

	// blobCache caches resolved blobs for futural use. This is especially useful when a layer
	// isn't eStargz/stargz (the *layer object won't be created/cached in this case).
	blobCache := lrucache.New(resolveResultEntry)
	blobCache.OnEvicted = func(key string, value interface{}) {
		if err := value.(remote.Blob).Close(); err != nil {
			logrus.WithField("key", key).WithError(err).Warnf("failed to clean up blob")
			return
		}
		logrus.WithField("key", key).Debugf("cleaned up blob")
	}

	// Prepare the generators of contents cache.
	httpCacheFactory := cacheFactory(filepath.Join(root, "httpcache"), cfg.HTTPCacheType, cfg)
	fsCacheFactory := cacheFactory(filepath.Join(root, "fscache"), cfg.FSCacheType, cfg)

	return &Resolver{
		resolver:              remote.NewResolver(httpCacheFactory, cfg.BlobConfig),
		fsCache:               fsCacheFactory,
		layerCache:            layerCache,
		blobCache:             blobCache,
		prefetchTimeout:       prefetchTimeout,
		backgroundTaskManager: backgroundTaskManager,
		resolveLock:           new(namedmutex.NamedMutex),
	}
}

// cacheFactory returns a callback that can be used to create a new content cache.
func cacheFactory(root string, cacheType string, cfg config.Config) func() (cache.BlobCache, error) {
	if cacheType == memoryCacheType {
		return func() (cache.BlobCache, error) {
			return cache.NewMemoryCache(), nil
		}
	}

	dcc := cfg.DirectoryCacheConfig
	maxDataEntry := dcc.MaxLRUCacheEntry
	if maxDataEntry == 0 {
		maxDataEntry = defaultMaxLRUCacheEntry
	}
	maxFdEntry := dcc.MaxCacheFds
	if maxFdEntry == 0 {
		maxFdEntry = defaultMaxCacheFds
	}

	// All directory caches share on-memory caches (pool and LRU caches).
	bufPool := &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	dCache, fCache := lrucache.New(maxDataEntry), lrucache.New(maxFdEntry)
	dCache.OnEvicted = func(key string, value interface{}) {
		bufPool.Put(value)
	}
	fCache.OnEvicted = func(key string, value interface{}) {
		value.(*os.File).Close()
	}
	return func() (cache.BlobCache, error) {
		// create a cache on an unique directory
		if err := os.MkdirAll(root, 0700); err != nil {
			return nil, err
		}
		cachePath, err := ioutil.TempDir(root, "")
		if err != nil {
			return nil, errors.Wrapf(err, "failed to initialize directory cache")
		}
		return cache.NewDirectoryCache(
			cachePath,
			cache.DirectoryCacheConfig{
				SyncAdd:   dcc.SyncAdd,
				DataCache: dCache,
				FdCache:   fCache,
				BufPool:   bufPool,
			},
		)
	}
}

// Resolve resolves a layer based on the passed layer blob information.
func (r *Resolver) Resolve(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) (_ Layer, retErr error) {
	name := refspec.String() + "/" + desc.Digest.String()

	// Wait if resolving this layer is already running. The result
	// can hopefully get from the LRU cache.
	r.resolveLock.Lock(name)
	defer r.resolveLock.Unlock(name)

	ctx, cancel := context.WithCancel(log.WithLogger(ctx, log.G(ctx).WithField("src", name)))
	defer cancel()

	// First, try to retrieve this layer from the underlying LRU cache.
	r.layerCacheMu.Lock()
	c, done, ok := r.layerCache.Get(name)
	r.layerCacheMu.Unlock()
	if ok {
		if l := c.(*layer); l.Check() == nil {
			log.G(ctx).Debugf("hit layer cache %q", name)
			return &layerRef{l, done}, nil
		}
		done()
		r.layerCacheMu.Lock()
		r.layerCache.Remove(name)
		r.layerCacheMu.Unlock()
	}

	log.G(ctx).Debugf("resolving")

	// Resolve the blob.
	blobR, err := r.resolveBlob(ctx, hosts, refspec, desc)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve the blob")
	}
	defer func() {
		if retErr != nil {
			blobR.done()
		}
	}()

	// Prepare the content cache for this layer.
	fsCache, err := r.fsCache()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to init cache")
	}
	defer func() {
		if retErr != nil {
			fsCache.Close()
		}
	}()

	// Get a reader for stargz archive.
	// Each file's read operation is a prioritized task and all background tasks
	// will be stopped during the execution so this can avoid being disturbed for
	// NW traffic by background tasks.
	sr := io.NewSectionReader(readerAtFunc(func(p []byte, offset int64) (n int, err error) {
		r.backgroundTaskManager.DoPrioritizedTask()
		defer r.backgroundTaskManager.DonePrioritizedTask()
		return blobR.ReadAt(p, offset)
	}), 0, blobR.Size())
	vr, root, err := reader.NewReader(sr, fsCache)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read layer")
	}

	// Combine layer information together and cache it.
	l := newLayer(r, desc, blobR, vr, root)
	r.layerCacheMu.Lock()
	cachedL, done2, added := r.layerCache.Add(name, l)
	r.layerCacheMu.Unlock()
	if !added {
		l.close() // layer already exists in the cache. discrad this.
	}

	log.G(ctx).Debugf("resolved")
	return &layerRef{cachedL.(*layer), done2}, nil
}

// resolveBlob resolves a blob based on the passed layer blob information.
func (r *Resolver) resolveBlob(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) (*blobRef, error) {
	name := refspec.String() + "/" + desc.Digest.String()

	// Try to retrieve the blob from the underlying LRU cache.
	r.blobCacheMu.Lock()
	c, done, ok := r.blobCache.Get(name)
	r.blobCacheMu.Unlock()
	if ok {
		if blob := c.(remote.Blob); blob.Check() == nil {
			return &blobRef{blob, done}, nil
		}
		// invalid blob. discard this.
		done()
		r.blobCacheMu.Lock()
		r.blobCache.Remove(name)
		r.blobCacheMu.Unlock()
	}

	// Resolve the blob and cache the result.
	b, err := r.resolver.Resolve(ctx, hosts, refspec, desc)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve the source")
	}
	r.blobCacheMu.Lock()
	cachedB, done, added := r.blobCache.Add(name, b)
	r.blobCacheMu.Unlock()
	if !added {
		b.Close() // blob already exists in the cache. discard this.
	}
	return &blobRef{cachedB.(remote.Blob), done}, nil
}

// Cache is similar to Resolve but the result isn't returned. Instead, it'll be stored in the cache.
func (r *Resolver) Cache(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error {
	l, err := r.Resolve(ctx, hosts, refspec, desc)
	if err != nil {
		return err
	}
	// Release this layer. However, this will remain on the cache until eviction.
	// Until then, the client can reuse this (already pre-resolved) layer.
	l.Done()
	return nil
}

// blobRef is a reference to the blob in the cache. Calling `done` decreases the reference counter
// of this blob in the underlying cache. When nobody refers to the blob in the cache, resources bound
// to this blob will be discarded.
type blobRef struct {
	remote.Blob
	done func()
}

// layerRef is a reference to the layer in the cache. Calling `Done` or `done` decreases the
// reference counter of this blob in the underlying cache. When nobody refers to the layer in the
// cache, resources bound to this layer will be discarded.
type layerRef struct {
	*layer
	done func()
}

func (l *layerRef) Done() {
	l.done()
}

func newLayer(
	resolver *Resolver,
	desc ocispec.Descriptor,
	blobRef *blobRef,
	vr *reader.VerifiableReader,
	root *estargz.TOCEntry,
) *layer {
	return &layer{
		resolver:         resolver,
		desc:             desc,
		blobRef:          blobRef,
		verifiableReader: vr,
		root:             root,
		prefetchWaiter:   newWaiter(),
	}
}

type layer struct {
	resolver         *Resolver
	desc             ocispec.Descriptor
	blobRef          *blobRef
	verifiableReader *reader.VerifiableReader
	root             *estargz.TOCEntry
	prefetchWaiter   *waiter
	closed           bool
	closedMu         sync.Mutex

	r reader.Reader
}

func (l *layer) Info() Info {
	return Info{
		Digest:      l.desc.Digest,
		Size:        l.blobRef.Size(),
		FetchedSize: l.blobRef.FetchedSize(),
	}
}

func (l *layer) Root() *estargz.TOCEntry {
	return l.root
}

func (l *layer) Check() error {
	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	return l.blobRef.Check()
}

func (l *layer) Refresh(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error {
	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	return l.blobRef.Refresh(ctx, hosts, refspec, desc)
}

func (l *layer) Verify(tocDigest digest.Digest) (err error) {
	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	l.r, err = l.verifiableReader.VerifyTOC(tocDigest)
	return
}

func (l *layer) SkipVerify() (err error) {
	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	l.r, err = l.verifiableReader.SkipVerify()
	return
}

func (l *layer) OpenFile(name string) (io.ReaderAt, error) {
	if l.isClosed() {
		return nil, fmt.Errorf("layer already closed")
	}
	if l.r == nil {
		return nil, fmt.Errorf("layer hasn't been verified yet")
	}
	return l.r.OpenFile(name)
}

func (l *layer) Prefetch(prefetchSize int64) error {
	defer l.prefetchWaiter.done() // Notify the completion

	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	if l.r == nil {
		return fmt.Errorf("layer hasn't been verified yet")
	}
	lr := l.r
	if _, ok := lr.Lookup(estargz.NoPrefetchLandmark); ok {
		// do not prefetch this layer
		return nil
	} else if e, ok := lr.Lookup(estargz.PrefetchLandmark); ok {
		// override the prefetch size with optimized value
		prefetchSize = e.Offset
	} else if prefetchSize > l.blobRef.Size() {
		// adjust prefetch size not to exceed the whole layer size
		prefetchSize = l.blobRef.Size()
	}

	// Fetch the target range
	if err := l.blobRef.Cache(0, prefetchSize); err != nil {
		return errors.Wrap(err, "failed to prefetch layer")
	}

	// Cache uncompressed contents of the prefetched range
	if err := lr.Cache(reader.WithFilter(func(e *estargz.TOCEntry) bool {
		return e.Offset < prefetchSize // Cache only prefetch target
	})); err != nil {
		return errors.Wrap(err, "failed to cache prefetched layer")
	}

	return nil
}

func (l *layer) WaitForPrefetchCompletion() error {
	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	return l.prefetchWaiter.wait(l.resolver.prefetchTimeout)
}

func (l *layer) BackgroundFetch() error {
	if l.isClosed() {
		return fmt.Errorf("layer already closed")
	}
	if l.r == nil {
		return fmt.Errorf("layer hasn't been verified yet")
	}
	lr := l.r
	br := io.NewSectionReader(readerAtFunc(func(p []byte, offset int64) (retN int, retErr error) {
		l.resolver.backgroundTaskManager.InvokeBackgroundTask(func(ctx context.Context) {
			retN, retErr = l.blobRef.ReadAt(
				p,
				offset,
				remote.WithContext(ctx),              // Make cancellable
				remote.WithCacheOpts(cache.Direct()), // Do not pollute mem cache
			)
		}, 120*time.Second)
		return
	}), 0, l.blobRef.Size())
	return lr.Cache(
		reader.WithReader(br),                // Read contents in background
		reader.WithCacheOpts(cache.Direct()), // Do not pollute mem cache
	)
}

func (l *layer) close() error {
	l.closedMu.Lock()
	defer l.closedMu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	defer l.blobRef.done() // Close reader first, then close the blob
	l.verifiableReader.Close()
	if l.r != nil {
		return l.r.Close()
	}
	return nil
}

func (l *layer) isClosed() bool {
	l.closedMu.Lock()
	closed := l.closed
	l.closedMu.Unlock()
	return closed
}

func newWaiter() *waiter {
	return &waiter{
		completionCond: sync.NewCond(&sync.Mutex{}),
	}
}

type waiter struct {
	isDone         bool
	isDoneMu       sync.Mutex
	completionCond *sync.Cond
}

func (w *waiter) done() {
	w.isDoneMu.Lock()
	w.isDone = true
	w.isDoneMu.Unlock()
	w.completionCond.Broadcast()
}

func (w *waiter) wait(timeout time.Duration) error {
	wait := func() <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			w.isDoneMu.Lock()
			isDone := w.isDone
			w.isDoneMu.Unlock()

			w.completionCond.L.Lock()
			if !isDone {
				w.completionCond.Wait()
			}
			w.completionCond.L.Unlock()
			ch <- struct{}{}
		}()
		return ch
	}
	select {
	case <-time.After(timeout):
		w.isDoneMu.Lock()
		w.isDone = true
		w.isDoneMu.Unlock()
		w.completionCond.Broadcast()
		return fmt.Errorf("timeout(%v)", timeout)
	case <-wait():
		return nil
	}
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, offset int64) (int, error) { return f(p, offset) }
