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

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/fs/layer"
	layermetrics "github.com/containerd/stargz-snapshotter/fs/metrics/layer"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/metadata"
	"github.com/containerd/stargz-snapshotter/task"
	"github.com/containerd/stargz-snapshotter/util/namedmutex"
	"github.com/docker/go-metrics"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
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
	r, err := layer.NewResolver(root, tm, cfg, nil, metadataStore) // TODO: support IPFS
	if err != nil {
		return nil, errors.Wrapf(err, "failed to setup resolver")
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
		allowNoVerification:   cfg.AllowNoVerification,
		disableVerification:   cfg.DisableVerification,
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
	allowNoVerification   bool
	disableVerification   bool
	metricsController     *layermetrics.Controller
	resolveLock           *namedmutex.NamedMutex

	layer      map[string]map[string]layer.Layer
	refcounter map[string]map[string]int

	mu sync.Mutex
}

func (r *LayerManager) cacheLayer(refspec reference.Spec, dgst digest.Digest, l layer.Layer) (_ layer.Layer, added bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.layer == nil {
		r.layer = make(map[string]map[string]layer.Layer)
	}
	if r.layer[refspec.String()] == nil {
		r.layer[refspec.String()] = make(map[string]layer.Layer)
	}
	if cl, ok := r.layer[refspec.String()][dgst.String()]; ok {
		return cl, false // already exists
	}
	r.layer[refspec.String()][dgst.String()] = l
	return l, true
}

func (r *LayerManager) getCachedLayer(refspec reference.Spec, dgst digest.Digest) layer.Layer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.layer == nil || r.layer[refspec.String()] == nil {
		return nil
	}
	if l, ok := r.layer[refspec.String()][dgst.String()]; ok {
		return l
	}
	return nil
}

func (r *LayerManager) getLayerInfo(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (Layer, error) {
	manifest, config, err := r.refPool.loadRef(ctx, refspec)
	if err != nil {
		return Layer{}, errors.Wrapf(err, "failed to get manifest and config")
	}
	return genLayerInfo(ctx, dgst, manifest, config)
}

func (r *LayerManager) getLayer(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (layer.Layer, error) {
	gotL := r.getCachedLayer(refspec, dgst)
	if gotL != nil {
		return gotL, nil
	}

	// resolve the layer and all other layers in the specified reference.
	var (
		result     layer.Layer
		resultChan = make(chan layer.Layer)
		errChan    = make(chan error)
	)
	manifest, _, err := r.refPool.loadRef(ctx, refspec)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get manifest and config")
	}
	var target ocispec.Descriptor
	var preResolve []ocispec.Descriptor
	var found bool
	for _, l := range manifest.Layers {
		if l.Digest == dgst {
			l := l
			found = true
			target = l
			continue
		}
		preResolve = append(preResolve, l)
	}
	if !found {
		return nil, fmt.Errorf("unknown digest %v for ref %q", target, refspec.String())
	}
	for _, l := range append([]ocispec.Descriptor{target}, preResolve...) {
		l := l

		// Check if layer is already resolved before creating goroutine.
		gotL := r.getCachedLayer(refspec, l.Digest)
		if gotL != nil {
			// Layer already resolved
			if l.Digest.String() != target.Digest.String() {
				continue // This is not the target layer; nop
			}
			result = gotL
			continue
		}

		// Resolve the layer
		go func() {
			// Avoids to get canceled by client.
			ctx := context.Background()
			gotL, err := r.resolveLayer(ctx, refspec, l)
			if l.Digest.String() != target.Digest.String() {
				return // This is not target layer
			}
			if err != nil {
				errChan <- errors.Wrapf(err, "failed to resolve layer %q / %q",
					refspec, l.Digest)
				return
			}
			// Log this as preparation success
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareSucceeded).Debugf("successfully resolved layer")
			resultChan <- gotL
		}()
	}

	if result != nil {
		return result, nil
	}

	// Wait for resolving completion
	var l layer.Layer
	select {
	case l = <-resultChan:
	case err := <-errChan:
		log.G(ctx).WithError(err).Debug("failed to resolve layer")
		return nil, errors.Wrapf(err, "failed to resolve layer")
	case <-time.After(30 * time.Second):
		log.G(ctx).Debug("failed to resolve layer (timeout)")
		return nil, fmt.Errorf("failed to resolve layer (timeout)")
	}

	return l, nil
}

func (r *LayerManager) resolveLayer(ctx context.Context, refspec reference.Spec, target ocispec.Descriptor) (layer.Layer, error) {
	key := refspec.String() + "/" + target.Digest.String()

	// Wait if resolving this layer is already running.
	r.resolveLock.Lock(key)
	defer r.resolveLock.Unlock(key)

	gotL := r.getCachedLayer(refspec, target.Digest)
	if gotL != nil {
		// layer already resolved
		return gotL, nil
	}

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
		return nil, err
	}

	// Verify layer's content
	labels := target.Annotations
	if labels == nil {
		labels = make(map[string]string)
	}
	if r.disableVerification {
		// Skip if verification is disabled completely
		l.SkipVerify()
		log.G(ctx).Debugf("Verification forcefully skipped")
	} else if tocDigest, ok := labels[estargz.TOCJSONDigestAnnotation]; ok {
		// Verify this layer using the TOC JSON digest passed through label.
		dgst, err := digest.Parse(tocDigest)
		if err != nil {
			log.G(ctx).WithError(err).Debugf("failed to parse passed TOC digest %q", dgst)
			return nil, errors.Wrapf(err, "invalid TOC digest: %v", tocDigest)
		}
		if err := l.Verify(dgst); err != nil {
			log.G(ctx).WithError(err).Debugf("invalid layer")
			return nil, errors.Wrapf(err, "invalid stargz layer")
		}
		log.G(ctx).Debugf("verified")
	} else {
		// Verification must be done. Don't mount this layer.
		return nil, fmt.Errorf("digest of TOC JSON must be passed")
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
	cachedL, added := r.cacheLayer(refspec, target.Digest, l)
	if added {
		r.metricsController.Add(key, cachedL)
	} else {
		l.Done() // layer is already cached. use the cached one instead. discard this layer.
	}

	return cachedL, nil
}

func (r *LayerManager) release(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (int, error) {
	r.refPool.release(refspec)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refcounter == nil || r.refcounter[refspec.String()] == nil {
		return 0, fmt.Errorf("ref %q not tracked", refspec.String())
	} else if _, ok := r.refcounter[refspec.String()][dgst.String()]; !ok {
		return 0, fmt.Errorf("layer %q/%q not tracked", refspec.String(), dgst.String())
	}
	r.refcounter[refspec.String()][dgst.String()]--
	i := r.refcounter[refspec.String()][dgst.String()]
	if i <= 0 {
		// No reference to this layer. release it.
		delete(r.refcounter, dgst.String())
		if len(r.refcounter[refspec.String()]) == 0 {
			delete(r.refcounter, refspec.String())
		}
		if r.layer == nil || r.layer[refspec.String()] == nil {
			return 0, fmt.Errorf("layer of reference %q is not registered (ref=%d)", refspec, i)
		}
		l, ok := r.layer[refspec.String()][dgst.String()]
		if !ok {
			return 0, fmt.Errorf("layer of digest %q/%q is not registered (ref=%d)", refspec, dgst, i)
		}
		l.Done()
		delete(r.layer[refspec.String()], dgst.String())
		if len(r.layer[refspec.String()]) == 0 {
			delete(r.layer, refspec.String())
		}
		log.G(ctx).WithField("refcounter", i).Infof("layer %v/%v is released due to no reference", refspec, dgst)
	}
	return i, nil
}

func (r *LayerManager) use(refspec reference.Spec, dgst digest.Digest) int {
	r.refPool.use(refspec)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refcounter == nil {
		r.refcounter = make(map[string]map[string]int)
	}
	if r.refcounter[refspec.String()] == nil {
		r.refcounter[refspec.String()] = make(map[string]int)
	}
	if _, ok := r.refcounter[refspec.String()][dgst.String()]; !ok {
		r.refcounter[refspec.String()][dgst.String()] = 1
		return 1
	}
	r.refcounter[refspec.String()][dgst.String()]++
	return r.refcounter[refspec.String()][dgst.String()]
}

func colon2dash(s string) string {
	return strings.ReplaceAll(s, ":", "-")
}

// Layer represents the layer information. Format is compatible to the one required by
// "additional layer store" of github.com/containers/storage.
type Layer struct {
	CompressedDigest   digest.Digest `json:"compressed-diff-digest,omitempty"`
	CompressedSize     int64         `json:"compressed-size,omitempty"`
	UncompressedDigest digest.Digest `json:"diff-digest,omitempty"`
	UncompressedSize   int64         `json:"diff-size,omitempty"`
	CompressionType    int           `json:"compression,omitempty"`
	ReadOnly           bool          `json:"-"`
}

// Defined in https://github.com/containers/storage/blob/b64e13a1afdb0bfed25601090ce4bbbb1bc183fc/pkg/archive/archive.go#L108-L119
const gzipTypeMagicNum = 2

func genLayerInfo(ctx context.Context, dgst digest.Digest, manifest ocispec.Manifest, config ocispec.Image) (Layer, error) {
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
	var uncompressedSize int64
	var err error
	if uncompressedSizeStr, ok := manifest.Layers[layerIndex].Annotations[estargz.StoreUncompressedSizeAnnotation]; ok {
		uncompressedSize, err = strconv.ParseInt(uncompressedSizeStr, 10, 64)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("layer %q has invalid uncompressed size; exposing incomplete layer info", dgst.String())
		}
	} else {
		log.G(ctx).Warnf("layer %q doesn't have uncompressed size; exposing incomplete layer info", dgst.String())
	}
	return Layer{
		CompressedDigest:   manifest.Layers[layerIndex].Digest,
		CompressedSize:     manifest.Layers[layerIndex].Size,
		UncompressedDigest: config.RootFS.DiffIDs[layerIndex],
		UncompressedSize:   uncompressedSize,
		CompressionType:    gzipTypeMagicNum,
		ReadOnly:           true,
	}, nil
}
