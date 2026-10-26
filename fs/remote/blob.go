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

package remote

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/cache"
	commonmetrics "github.com/containerd/stargz-snapshotter/fs/metrics/common"
	"github.com/containerd/stargz-snapshotter/fs/source"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

var contentRangeRegexp = regexp.MustCompile(`bytes ([0-9]+)-([0-9]+)/([0-9]+|\\*)`)

type Blob interface {
	Check() error
	Size() int64
	FetchedSize() int64
	ReadAt(p []byte, offset int64, opts ...Option) (int, error)
	Cache(offset int64, size int64, opts ...Option) error
	Refresh(ctx context.Context, host source.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error
	Close() error
}

type blob struct {
	fetcher   fetcher
	fetcherMu sync.Mutex

	size              int64
	chunkSize         int64
	prefetchChunkSize int64
	cache             cache.BlobCache
	lastCheck         time.Time
	lastCheckMu       sync.Mutex
	checkInterval     time.Duration
	fetchTimeout      time.Duration

	fetchedRegionSet    regionSet
	fetchedRegionSetMu  sync.Mutex
	fetchedRegionGroup  singleflight.Group
	fetchedRegionCopyMu sync.Mutex

	resolver *Resolver

	// layer labels the skipped-caching counter; empty when not known.
	layer digest.Digest

	// Rate limits the warning logged when a chunk is served without being
	// cached, which under a full cache would otherwise be logged per chunk.
	skipLogMu      sync.Mutex
	skipLogLast    time.Time
	skipLogDropped int

	closed   bool
	closedMu sync.Mutex
}

func makeBlob(fetcher fetcher, size int64, chunkSize int64, prefetchChunkSize int64,
	blobCache cache.BlobCache, lastCheck time.Time, checkInterval time.Duration,
	r *Resolver, fetchTimeout time.Duration) *blob {
	return &blob{
		fetcher:           fetcher,
		size:              size,
		chunkSize:         chunkSize,
		prefetchChunkSize: prefetchChunkSize,
		cache:             blobCache,
		lastCheck:         lastCheck,
		checkInterval:     checkInterval,
		resolver:          r,
		fetchTimeout:      fetchTimeout,
	}
}

func (b *blob) Close() error {
	b.closedMu.Lock()
	defer b.closedMu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	return b.cache.Close()
}

func (b *blob) isClosed() bool {
	b.closedMu.Lock()
	closed := b.closed
	b.closedMu.Unlock()
	return closed
}

func (b *blob) Refresh(ctx context.Context, hosts source.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error {
	if b.isClosed() {
		return fmt.Errorf("blob is already closed")
	}

	// refresh the fetcher
	f, newSize, err := b.resolver.resolveFetcher(ctx, hosts, refspec, desc)
	if err != nil {
		return err
	}
	if newSize != b.size {
		return fmt.Errorf("invalid size of new blob %d; want %d", newSize, b.size)
	}

	// update the blob's fetcher with new one
	b.fetcherMu.Lock()
	b.fetcher = f
	b.fetcherMu.Unlock()
	b.lastCheckMu.Lock()
	b.lastCheck = time.Now()
	b.lastCheckMu.Unlock()

	return nil
}

func (b *blob) Check() error {
	if b.isClosed() {
		return fmt.Errorf("blob is already closed")
	}

	now := time.Now()
	b.lastCheckMu.Lock()
	lastCheck := b.lastCheck
	b.lastCheckMu.Unlock()
	if now.Sub(lastCheck) < b.checkInterval {
		// do nothing if not expired
		return nil
	}
	b.fetcherMu.Lock()
	fr := b.fetcher
	b.fetcherMu.Unlock()
	err := fr.check()
	if err == nil {
		// update lastCheck only if check succeeded.
		// on failure, we should check this layer next time again.
		b.lastCheckMu.Lock()
		b.lastCheck = now
		b.lastCheckMu.Unlock()
	}

	return err
}

func (b *blob) Size() int64 {
	return b.size
}

func (b *blob) FetchedSize() int64 {
	b.fetchedRegionSetMu.Lock()
	sz := b.fetchedRegionSet.totalSize()
	b.fetchedRegionSetMu.Unlock()
	return sz
}

func makeSyncKey(allData map[region]io.Writer) string {
	keys := make([]string, len(allData))
	keysIndex := 0
	for key := range allData {
		keys[keysIndex] = fmt.Sprintf("[%d,%d]", key.b, key.e)
		keysIndex++
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (b *blob) cacheAt(offset int64, size int64, fr fetcher, cacheOpts *options) error {
	fetchReg := region{floor(offset, b.chunkSize), ceil(offset+size-1, b.chunkSize) - 1}
	discard := make(map[region]io.Writer)

	err := b.walkChunks(fetchReg, func(reg region) error {
		if r, err := b.cache.Get(fr.genID(reg), cacheOpts.cacheOpts...); err == nil {
			return r.Close() // nop if the cache hits
		}
		discard[reg] = io.Discard
		return nil
	})
	if err != nil {
		return err
	}

	return b.fetchRange(discard, cacheOpts)
}

func (b *blob) Cache(offset int64, size int64, opts ...Option) error {
	if b.isClosed() {
		return fmt.Errorf("blob is already closed")
	}

	var cacheOpts options
	for _, o := range opts {
		o(&cacheOpts)
	}

	b.fetcherMu.Lock()
	fr := b.fetcher
	b.fetcherMu.Unlock()

	if b.prefetchChunkSize <= b.chunkSize {
		return b.cacheAt(offset, size, fr, &cacheOpts)
	}

	eg, _ := errgroup.WithContext(context.Background())

	fetchSize := b.chunkSize * (b.prefetchChunkSize / b.chunkSize)

	end := offset + size
	for i := offset; i < end; i += fetchSize {
		i, l := i, fetchSize
		if i+l > end {
			l = end - i
		}
		eg.Go(func() error {
			return b.cacheAt(i, l, fr, &cacheOpts)
		})
	}

	return eg.Wait()
}

// ReadAt reads remote chunks from specified offset for the buffer size.
// It tries to fetch as many chunks as possible from local cache.
// We can configure this function with options.
func (b *blob) ReadAt(p []byte, offset int64, opts ...Option) (int, error) {
	if b.isClosed() {
		return 0, fmt.Errorf("blob is already closed")
	}

	if len(p) == 0 || offset > b.size {
		return 0, nil
	}

	// Make the buffer chunk aligned
	allRegion := region{floor(offset, b.chunkSize), ceil(offset+int64(len(p))-1, b.chunkSize) - 1}
	allData := make(map[region]io.Writer)

	var readAtOpts options
	for _, o := range opts {
		o(&readAtOpts)
	}

	fr := b.getFetcher()

	if err := b.prepareChunksForRead(allRegion, offset, p, fr, allData, &readAtOpts); err != nil {
		return 0, err
	}

	// Read required data
	if err := b.fetchRange(allData, &readAtOpts); err != nil {
		return 0, err
	}

	return b.adjustBufferSize(p, offset), nil
}

// prepareChunksForRead prepares chunks for reading by checking cache and setting up writers
func (b *blob) prepareChunksForRead(allRegion region, offset int64, p []byte, fr fetcher, allData map[region]io.Writer, opts *options) error {
	return b.walkChunks(allRegion, func(chunk region) error {
		var (
			base         = positive(chunk.b - offset)
			lowerUnread  = positive(offset - chunk.b)
			upperUnread  = positive(chunk.e + 1 - (offset + int64(len(p))))
			expectedSize = chunk.size() - upperUnread - lowerUnread
		)

		// Try to read from cache first
		if err := b.readFromCache(chunk, p[base:base+expectedSize], lowerUnread, fr, opts); err == nil {
			return nil
		}

		// We missed cache. Take it from remote registry.
		// We get the whole chunk here and add it to the cache so that following
		// reads against neighboring chunks can take the data without making HTTP requests.
		allData[chunk] = newBytesWriter(p[base:base+expectedSize], lowerUnread)
		return nil
	})
}

// readFromCache attempts to read chunk data from cache
func (b *blob) readFromCache(chunk region, dest []byte, offset int64, fr fetcher, opts *options) error {
	r, err := b.cache.Get(fr.genID(chunk), opts.cacheOpts...)
	if err != nil {
		return err
	}
	defer r.Close()
	n, err := r.ReadAt(dest, offset)
	if err != nil && err != io.EOF {
		return err
	}
	if n != len(dest) {
		return fmt.Errorf("incomplete read from cache: read %d bytes, expected %d bytes", n, len(dest))
	}
	return nil
}

// fetchRegions fetches all specified chunks from remote blob and puts it in the local cache.
// It must be called from within fetchRange and need to ensure that it is inside the singleflight `Do` operation.
func (b *blob) fetchRegions(allData map[region]io.Writer, fetched map[region]bool, opts *options) error {
	if len(allData) == 0 {
		return nil
	}

	fr := b.getFetcher()

	// request missed regions
	var req []region
	for reg := range allData {
		req = append(req, reg)
		fetched[reg] = false
	}

	fetchCtx, cancel := context.WithTimeout(context.Background(), b.fetchTimeout)
	defer cancel()
	if opts.ctx != nil {
		fetchCtx = opts.ctx
	}
	mr, err := fr.fetch(fetchCtx, req, true)
	if err != nil {
		return err
	}
	defer mr.Close()

	// Update the check timer because we succeeded to access the blob
	b.lastCheckMu.Lock()
	b.lastCheck = time.Now()
	b.lastCheckMu.Unlock()

	// chunk and cache responsed data. Regions must be aligned by chunk size.
	// TODO: Reorganize remoteData to make it be aligned by chunk size
	for {
		reg, p, err := mr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("failed to read multipart resp: %w", err)
		}
		if err := b.walkChunks(reg, func(chunk region) (retErr error) {
			if err := b.cacheChunkData(chunk, p, fr, allData, fetched, opts); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to get chunks: %w", err)
		}
	}

	// Check all chunks are fetched
	var unfetched []region
	for c, b := range fetched {
		if !b {
			unfetched = append(unfetched, c)
		}
	}
	if unfetched != nil {
		return fmt.Errorf("failed to fetch region %v", unfetched)
	}

	return nil
}

// fetchRangeSharedRetries bounds how many times a caller that joined another
// caller's fetch retries when the result is not in the cache to copy. A chunk
// that could not be cached is served only to the caller that fetched it, so
// retrying may never find it.
const fetchRangeSharedRetries = 2

// fetchRange fetches all specified chunks from local cache and remote blob.
func (b *blob) fetchRange(allData map[region]io.Writer, opts *options) error {
	if len(allData) == 0 {
		return nil
	}

	key := makeSyncKey(allData)
	for range fetchRangeSharedRetries {
		fetched := make(map[region]bool)
		_, err, shared := b.fetchedRegionGroup.Do(key, func() (any, error) {
			return nil, b.fetchRegions(allData, fetched, opts)
		})
		if err != nil || !shared {
			return err
		}
		// When unblocked try to read from cache in case if there were no errors
		// If we fail reading from cache, fetch from remote registry again
		if err := b.handleSharedFetch(allData, fetched, opts); err == nil {
			return nil
		}
	}

	// The shared result keeps missing from the cache, so fetch this range
	// without joining anyone, at the cost of a duplicate range request.
	return b.fetchRegions(allData, make(map[region]bool), opts)
}

// handleSharedFetch handles the case when multiple goroutines share the same fetch result
func (b *blob) handleSharedFetch(allData map[region]io.Writer, fetched map[region]bool, opts *options) error {
	for reg := range allData {
		if _, ok := fetched[reg]; ok {
			continue
		}
		if err := b.copyFetchedChunks(reg, allData, opts); err != nil {
			return err
		}
	}
	return nil
}

// copyFetchedChunks copies fetched chunks from cache to target writer
func (b *blob) copyFetchedChunks(reg region, allData map[region]io.Writer, opts *options) error {
	return b.walkChunks(reg, func(chunk region) error {
		fr := b.getFetcher()
		r, err := b.cache.Get(fr.genID(chunk), opts.cacheOpts...)
		if err != nil {
			return err
		}
		defer r.Close()

		b.fetchedRegionCopyMu.Lock()
		defer b.fetchedRegionCopyMu.Unlock()

		if _, err := io.CopyN(allData[chunk], io.NewSectionReader(r, 0, chunk.size()), chunk.size()); err != nil {
			return err
		}
		return nil
	})
}

// getFetcher safely gets the current fetcher
// Fetcher can be suddenly updated so we take and use the snapshot of it for consistency.
func (b *blob) getFetcher() fetcher {
	b.fetcherMu.Lock()
	defer b.fetcherMu.Unlock()
	return b.fetcher
}

// adjustBufferSize adjusts buffer size according to the blob size
func (b *blob) adjustBufferSize(p []byte, offset int64) int {
	if remain := b.size - offset; int64(len(p)) >= remain {
		if remain < 0 {
			remain = 0
		}
		p = p[:remain]
	}
	return len(p)
}

type walkFunc func(reg region) error

// walkChunks walks chunks from begin to end in order in the specified region.
// specified region must be aligned by chunk size.
func (b *blob) walkChunks(allRegion region, walkFn walkFunc) error {
	if allRegion.b%b.chunkSize != 0 {
		return fmt.Errorf("region (%d, %d) must be aligned by chunk size",
			allRegion.b, allRegion.e)
	}
	for i := allRegion.b; i <= allRegion.e && i < b.size; i += b.chunkSize {
		reg := region{i, i + b.chunkSize - 1}
		if reg.e >= b.size {
			reg.e = b.size - 1
		}
		if err := walkFn(reg); err != nil {
			return err
		}
	}
	return nil
}

func newBytesWriter(dest []byte, destOff int64) io.Writer {
	return &bytesWriter{
		dest:    dest,
		destOff: destOff,
		current: 0,
	}
}

type bytesWriter struct {
	dest    []byte
	destOff int64
	current int64
}

func (bw *bytesWriter) Write(p []byte) (int, error) {
	defer func() { bw.current = bw.current + int64(len(p)) }()

	var (
		destBase = positive(bw.current - bw.destOff)
		pBegin   = positive(bw.destOff - bw.current)
		pEnd     = positive(bw.destOff + int64(len(bw.dest)) - bw.current)
	)

	if destBase > int64(len(bw.dest)) {
		return len(p), nil
	}
	if pBegin >= int64(len(p)) {
		return len(p), nil
	}
	if pEnd > int64(len(p)) {
		pEnd = int64(len(p))
	}

	copy(bw.dest[destBase:], p[pBegin:pEnd])

	return len(p), nil
}

func floor(n int64, unit int64) int64 {
	return (n / unit) * unit
}

func ceil(n int64, unit int64) int64 {
	return (n/unit + 1) * unit
}

func positive(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// tolerantWriter passes writes through to w, records the first error instead
// of returning it, and always reports the full write. io.MultiWriter stops at
// the first failing writer, so without this a failing cache write would keep
// the fetched bytes from reaching the caller's buffer.
type tolerantWriter struct {
	w   io.Writer
	err error
}

func (t *tolerantWriter) Write(p []byte) (int, error) {
	if t.err == nil {
		if _, err := t.w.Write(p); err != nil {
			t.err = err
		}
	}
	return len(p), nil
}

// cacheChunkData caches a fetched chunk and, if the caller asked for it, copies
// it to the caller's buffer. Failing to cache does not fail the read: the chunk
// is served and not kept. Only a short response body is an error.
func (b *blob) cacheChunkData(chunk region, r io.Reader, fr fetcher, allData map[region]io.Writer, fetched map[region]bool, opts *options) error {
	var dest io.Writer
	if _, ok := fetched[chunk]; ok {
		dest = allData[chunk]
	}

	id := fr.genID(chunk)
	cw, err := b.cache.Add(id, opts.cacheOpts...)
	if err != nil {
		// The chunk's bytes must still be consumed, or every following
		// chunk in the response body would be read at the wrong offset.
		if dest == nil {
			dest = io.Discard
		}
		if _, err := io.CopyN(dest, r, chunk.size()); err != nil {
			return fmt.Errorf("failed to read chunk data: %w", err)
		}
		b.skippedCaching(chunk, fetched, opts, fmt.Errorf("failed to create cache writer: %w", err))
		return nil
	}
	defer cw.Close()

	tw := &tolerantWriter{w: cw}
	w := io.Writer(tw)
	if dest != nil {
		w = io.MultiWriter(tw, dest)
	}

	if _, err := io.CopyN(w, r, chunk.size()); err != nil {
		cw.Abort()
		return fmt.Errorf("failed to write chunk data: %w", err)
	}
	if tw.err != nil {
		cw.Abort()
		b.skippedCaching(chunk, fetched, opts, fmt.Errorf("failed to write chunk data: %w", tw.err))
		return nil
	}

	if err := cw.Commit(); err != nil {
		cw.Abort()
		b.skippedCaching(chunk, fetched, opts, fmt.Errorf("failed to commit chunk: %w", err))
		return nil
	}

	b.fetchedRegionSetMu.Lock()
	b.fetchedRegionSet.add(chunk)
	b.fetchedRegionSetMu.Unlock()
	fetched[chunk] = true

	return nil
}

// skipLogInterval is the minimum interval between warnings about chunks served
// without being cached, per blob.
const skipLogInterval = time.Minute

// skippedCaching records a chunk that was served to the caller but not cached.
// It is marked fetched so that fetchRegions does not fail the read for it, but
// is left out of fetchedRegionSet because it is not in the cache.
func (b *blob) skippedCaching(chunk region, fetched map[region]bool, opts *options, cause error) {
	fetched[chunk] = true
	commonmetrics.IncCacheWriteSkipped(b.layer)

	b.skipLogMu.Lock()
	if time.Since(b.skipLogLast) < skipLogInterval {
		b.skipLogDropped++
		b.skipLogMu.Unlock()
		return
	}
	dropped := b.skipLogDropped
	b.skipLogLast, b.skipLogDropped = time.Now(), 0
	b.skipLogMu.Unlock()

	ctx := opts.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log.G(ctx).WithError(cause).
		WithField("layer", b.layer.String()).
		WithField("chunk", fmt.Sprintf("%d-%d", chunk.b, chunk.e)).
		WithField("suppressed", dropped).
		Warn("serving chunk without caching it")
}
