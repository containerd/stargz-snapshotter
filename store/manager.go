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

package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/fs/layer"
	layermetrics "github.com/containerd/stargz-snapshotter/fs/metrics/layer"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/metadata"
	esgzexternaltoc "github.com/containerd/stargz-snapshotter/nativeconverter/estargz/externaltoc"
	"github.com/containerd/stargz-snapshotter/task"
	"github.com/containerd/stargz-snapshotter/util/namedmutex"
	"github.com/docker/go-metrics"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	remoteSnapshotLogKey = "remote-snapshot-prepared"
	prepareSucceeded     = "true"
	prepareFailed        = "false"

	defaultMaxConcurrency = 2
)

func NewLayerManager(ctx context.Context, root string, hosts source.RegistryHosts, metadataStore metadata.Store, cfg config.Config) (*LayerManager, error) {
	refPool, err := newRefPool(ctx, root, hosts)
	if err != nil {
		return nil, err
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency == 0 {
		maxConcurrency = defaultMaxConcurrency
	}
	tm := task.NewBackgroundTaskManager(maxConcurrency, 5*time.Second)
	r, err := layer.NewResolver(root, tm, cfg, nil, metadataStore, layer.OverlayOpaqueAll,
		func(ctx context.Context, hosts source.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) []metadata.Decompressor {
			return []metadata.Decompressor{esgzexternaltoc.NewRemoteDecompressor(ctx, hosts, refspec, desc)}
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to setup resolver: %w", err)
	}
	var ns *metrics.Namespace
	if !cfg.NoPrometheus {
		ns = metrics.NewNamespace("stargz", "fs", nil)
	}
	c := layermetrics.NewLayerMetrics(ns)
	if ns != nil {
		metrics.Register(ns)
	}
	return &LayerManager{
		refPool:               refPool,
		hosts:                 hosts,
		resolver:              r,
		prefetchSize:          cfg.PrefetchSize,
		noprefetch:            cfg.NoPrefetch,
		noBackgroundFetch:     cfg.NoBackgroundFetch,
		backgroundTaskManager: tm,
		metricsController:     c,
		resolveLock:           new(namedmutex.NamedMutex),
		layer:                 make(map[string]map[string]layer.Layer),
		refcounter:            make(map[string]map[string]int),
	}, nil
}

// LayerManager manages layers of images and their resource lifetime.
type LayerManager struct {
	refPool *refPool
	hosts   source.RegistryHosts

	resolver              *layer.Resolver
	prefetchSize          int64
	noprefetch            bool
	noBackgroundFetch     bool
	backgroundTaskManager *task.BackgroundTaskManager
	metricsController     *layermetrics.Controller
	resolveLock           *namedmutex.NamedMutex

	layer      map[string]map[string]layer.Layer // keyed by image ref and TOC Digest
	refcounter map[string]map[string]int         // keyed by image ref and TOC Digest

	resolveLayerCache map[string]map[string]error // keyed by image ref and layer digest (not TOCDigest)

	mu sync.Mutex
}

func (r *LayerManager) cacheLayer(refspec reference.Spec, tocDigest digest.Digest, l layer.Layer) (_ layer.Layer, added bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.layer == nil {
		r.layer = make(map[string]map[string]layer.Layer)
	}
	if r.layer[refspec.String()] == nil {
		r.layer[refspec.String()] = make(map[string]layer.Layer)
	}
	if cl, ok := r.layer[refspec.String()][tocDigest.String()]; ok && cl.Info().TOCDigest == tocDigest {
		return cl, false // already exists
	}
	r.layer[refspec.String()][tocDigest.String()] = l
	return l, true
}

func (r *LayerManager) getCachedLayer(refspec reference.Spec, tocDigest digest.Digest) layer.Layer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.layer == nil || r.layer[refspec.String()] == nil {
		return nil
	}
	if l, ok := r.layer[refspec.String()][tocDigest.String()]; ok && l.Info().TOCDigest == tocDigest {
		return l
	}
	return nil
}

func (r *LayerManager) getLayerInfo(ctx context.Context, refspec reference.Spec, tocDigest digest.Digest) (Layer, error) {
	manifest, config, err := r.refPool.loadRef(ctx, refspec)
	if err != nil {
		return Layer{}, fmt.Errorf("failed to get manifest and config: %w", err)
	}
	gotL := r.getCachedLayer(refspec, tocDigest)
	if gotL == nil {
		return Layer{TOCDigest: tocDigest}, nil
	}
	return genLayerInfo(ctx, gotL.Info().Digest, manifest, config, tocDigest)
}

func (r *LayerManager) getLayer(ctx context.Context, refspec reference.Spec, tocDigest digest.Digest) (layer.Layer, error) {
	gotL := r.getCachedLayer(refspec, tocDigest)
	if gotL != nil {
		return gotL, nil
	}

	// resolve the layer and all other layers in the specified reference.
	var (
		result     layer.Layer
		resultChan = make(chan layer.Layer)
		errChan    = make(chan error)
		wg         sync.WaitGroup
	)
	manifest, _, err := r.refPool.loadRef(ctx, refspec)
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest and config: %w", err)
	}
	for _, l := range manifest.Layers {
		l := l

		// Resolve the layer
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Avoids to get canceled by client.
			ctx := context.Background()
			if err := r.resolveLayer(ctx, refspec, l); err != nil {
				log.G(ctx).Debugf("failed to resolve layer %q / %q: %v", refspec, l.Digest, err)
				return
			}

			gotL := r.getCachedLayer(refspec, tocDigest) // Try to get the layer we want.
			if gotL == nil {
				return // Layer not found
			}

			// Log this as preparation success
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareSucceeded).Debugf("successfully resolved layer")
			resultChan <- gotL
		}()
	}

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	if result != nil {
		return result, nil
	}

	// Wait for resolving completion
	var l layer.Layer
	select {
	case l = <-resultChan:
	case err := <-errChan:
		log.G(ctx).WithError(err).Debug("failed to resolve layer")
		return nil, fmt.Errorf("failed to resolve layer: %w", err)
	case <-time.After(30 * time.Second):
		log.G(ctx).Debug("failed to resolve layer (timeout)")
		return nil, fmt.Errorf("failed to resolve layer (timeout)")
	case <-allDone:
		log.G(ctx).Debug("fetched all layer but no layer with the expected TOC acquired")
		return nil, fmt.Errorf("layer with TOCDigest %v not found", tocDigest)
	}

	return l, nil
}

func (r *LayerManager) resolveLayer(ctx context.Context, refspec reference.Spec, target ocispec.Descriptor) (retErr error) {
	key := refspec.String() + "/" + target.Digest.String()

	// Wait if resolving this layer is already running.
	r.resolveLock.Lock(key)
	defer r.resolveLock.Unlock(key)

	// Check if this resolving has already done.
	r.mu.Lock()
	if r.resolveLayerCache != nil && r.resolveLayerCache[refspec.String()] != nil {
		if err, ok := r.resolveLayerCache[refspec.String()][target.Digest.String()]; ok {
			r.mu.Unlock()
			return err
		}
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.resolveLayerCache == nil {
			r.resolveLayerCache = make(map[string]map[string]error)
		}
		if r.resolveLayerCache[refspec.String()] == nil {
			r.resolveLayerCache[refspec.String()] = make(map[string]error)
		}
		r.resolveLayerCache[refspec.String()][target.Digest.String()] = retErr
		r.mu.Unlock()
	}()

	// Resolve this layer.
	var esgzOpts []metadata.Option
	if target.Annotations != nil {
		if tocOffsetStr, ok := target.Annotations[zstdchunked.ManifestPositionAnnotation]; ok {
			if parts := strings.Split(tocOffsetStr, ":"); len(parts) == 4 {
				tocOffset, err := strconv.ParseInt(parts[0], 10, 64)
				if err == nil {
					esgzOpts = append(esgzOpts, metadata.WithTOCOffset(tocOffset))
				}
			}
		}
	}
	l, err := r.resolver.Resolve(ctx, r.hosts, refspec, target, esgzOpts...)
	if err != nil {
		return err
	}
	// Prefetch this layer. We prefetch several layers in parallel. The first
	// Check() for this layer waits for the prefetch completion.
	if !r.noprefetch {
		go func() {
			r.backgroundTaskManager.DoPrioritizedTask()
			defer r.backgroundTaskManager.DonePrioritizedTask()
			if err := l.Prefetch(r.prefetchSize); err != nil {
				log.G(ctx).WithError(err).Debug("failed to prefetched layer")
				return
			}
			log.G(ctx).Debug("completed to prefetch")
		}()
	}

	// Fetch whole layer aggressively in background. We use background
	// reader for this so prioritized tasks(Mount, Check, etc...) can
	// interrupt the reading. This can avoid disturbing prioritized tasks
	// about NW traffic.
	if !r.noBackgroundFetch {
		go func() {
			if err := l.BackgroundFetch(); err != nil {
				log.G(ctx).WithError(err).Debug("failed to fetch whole layer")
				return
			}
			log.G(ctx).Debug("completed to fetch all layer data in background")
		}()
	}

	// Cache this layer.
	cachedL, added := r.cacheLayer(refspec, l.Info().TOCDigest, l)
	if added {
		r.metricsController.Add(key, cachedL)
	} else {
		l.Done() // layer is already cached. use the cached one instead. discard this layer.
	}

	return nil
}

func (r *LayerManager) release(ctx context.Context, refspec reference.Spec, tocDigest digest.Digest) (int, error) {
	r.refPool.release(refspec)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refcounter == nil || r.refcounter[refspec.String()] == nil {
		return 0, fmt.Errorf("ref %q not tracked", refspec.String())
	} else if _, ok := r.refcounter[refspec.String()][tocDigest.String()]; !ok {
		return 0, fmt.Errorf("layer %q/%q not tracked", refspec.String(), tocDigest.String())
	}
	r.refcounter[refspec.String()][tocDigest.String()]--
	i := r.refcounter[refspec.String()][tocDigest.String()]
	if i <= 0 {
		// No reference to this layer. release it.
		delete(r.refcounter, tocDigest.String())
		if len(r.refcounter[refspec.String()]) == 0 {
			delete(r.refcounter, refspec.String())
			delete(r.resolveLayerCache, refspec.String()) // no reference to this image. So reset the resolve status as well.
		}
		if r.layer == nil || r.layer[refspec.String()] == nil {
			return 0, fmt.Errorf("layer of reference %q is not registered (ref=%d)", refspec, i)
		}
		l, ok := r.layer[refspec.String()][tocDigest.String()]
		if !ok {
			return 0, fmt.Errorf("layer of digest %q/%q is not registered (ref=%d)", refspec, tocDigest, i)
		}
		l.Done()
		delete(r.layer[refspec.String()], tocDigest.String())
		if len(r.layer[refspec.String()]) == 0 {
			delete(r.layer, refspec.String())
		}
		log.G(ctx).WithField("refcounter", i).Infof("layer %v/%v is released due to no reference", refspec, tocDigest)
	}
	return i, nil
}

func (r *LayerManager) use(refspec reference.Spec, tocDigest digest.Digest) int {
	r.refPool.use(refspec)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refcounter == nil {
		r.refcounter = make(map[string]map[string]int)
	}
	if r.refcounter[refspec.String()] == nil {
		r.refcounter[refspec.String()] = make(map[string]int)
	}
	if _, ok := r.refcounter[refspec.String()][tocDigest.String()]; !ok {
		r.refcounter[refspec.String()][tocDigest.String()] = 1
		return 1
	}
	r.refcounter[refspec.String()][tocDigest.String()]++
	return r.refcounter[refspec.String()][tocDigest.String()]
}

func colon2dash(s string) string {
	return strings.ReplaceAll(s, ":", "-")
}

// Layer represents the layer information. Format is compatible to the one required by
// "additional layer store" of github.com/containers/storage.
type Layer struct {
	UncompressedSize int64             `json:"diff-size,omitempty"`
	CompressionType  int               `json:"compression,omitempty"`
	TOCDigest        digest.Digest     `json:"toc-digest,omitempty"`
	Flags            map[string]string `json:"flags,omitempty"`
}

// Defined in https://github.com/containers/storage/blob/b64e13a1afdb0bfed25601090ce4bbbb1bc183fc/pkg/archive/archive.go#L108-L119
const gzipTypeMagicNum = 2

func genLayerInfo(ctx context.Context, dgst digest.Digest, manifest ocispec.Manifest, config ocispec.Image, tocDigest digest.Digest) (Layer, error) {
	if len(manifest.Layers) != len(config.RootFS.DiffIDs) {
		return Layer{}, fmt.Errorf(
			"len(manifest.Layers) != len(config.Rootfs): %d != %d",
			len(manifest.Layers), len(config.RootFS.DiffIDs))
	}
	var (
		layerIndex = -1
	)
	for i, l := range manifest.Layers {
		if l.Digest == dgst {
			layerIndex = i
		}
	}
	if layerIndex == -1 {
		return Layer{}, fmt.Errorf("layer %q not found in the manifest", dgst.String())
	}

	layerFlags := map[string]string{
		"expected-layer-diffid": config.RootFS.DiffIDs[layerIndex].String(),
	}
	return Layer{
		UncompressedSize: -1, // means unknown
		CompressionType:  gzipTypeMagicNum,
		TOCDigest:        tocDigest,
		Flags:            layerFlags,
	}, nil
}
