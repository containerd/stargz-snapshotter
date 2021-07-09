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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/fs/layer"
	layermetrics "github.com/containerd/stargz-snapshotter/fs/metrics/layer"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/task"
	"github.com/containerd/stargz-snapshotter/util/namedmutex"
	"github.com/docker/go-metrics"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	// remoteSnapshotLogKey is a key for log line, which indicates whether
	// `Prepare` method successfully prepared targeting remote snapshot or not, as
	// defined in the following:
	// - "true"  : indicates the snapshot has been successfully prepared as a
	//             remote snapshot
	// - "false" : indicates the snapshot failed to be prepared as a remote
	//             snapshot
	// - null    : undetermined
	remoteSnapshotLogKey = "remote-snapshot-prepared"
	prepareSucceeded     = "true"
	prepareFailed        = "false"

	defaultMaxConcurrency = 2
)

func NewPool(root string, hosts source.RegistryHosts, cfg config.Config) (*Pool, error) {
	var poolroot = filepath.Join(root, "pool")
	if err := os.MkdirAll(poolroot, 0700); err != nil {
		return nil, err
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency == 0 {
		maxConcurrency = defaultMaxConcurrency
	}
	tm := task.NewBackgroundTaskManager(maxConcurrency, 5*time.Second)
	r, err := layer.NewResolver(root, tm, cfg)
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
	return &Pool{
		path:                  poolroot,
		layer:                 make(map[string]layer.Layer),
		hosts:                 hosts,
		refcounter:            make(map[string]map[string]int),
		resolver:              r,
		prefetchSize:          cfg.PrefetchSize,
		noprefetch:            cfg.NoPrefetch,
		noBackgroundFetch:     cfg.NoBackgroundFetch,
		backgroundTaskManager: tm,
		allowNoVerification:   cfg.AllowNoVerification,
		disableVerification:   cfg.DisableVerification,
		metricsController:     c,
		resolveLock:           new(namedmutex.NamedMutex),
	}, nil
}

// resolver provides manifests, configs and layers of images.
// This also manages caches for these resources.
type Pool struct {
	path         string
	layer        map[string]layer.Layer
	layerMu      sync.Mutex
	hosts        source.RegistryHosts
	refcounter   map[string]map[string]int
	refcounterMu sync.Mutex

	resolver              *layer.Resolver
	prefetchSize          int64
	noprefetch            bool
	noBackgroundFetch     bool
	backgroundTaskManager *task.BackgroundTaskManager
	allowNoVerification   bool
	disableVerification   bool
	metricsController     *layermetrics.Controller
	resolveLock           *namedmutex.NamedMutex
}

func (p *Pool) root() string {
	return p.path
}

func (p *Pool) metadataDir(refspec reference.Spec) string {
	return filepath.Join(p.path,
		"metadata--"+colon2dash(digest.FromString(refspec.String()).String()))
}

func (p *Pool) manifestFile(refspec reference.Spec) string {
	return filepath.Join(p.metadataDir(refspec), "manifest")
}

func (p *Pool) configFile(refspec reference.Spec) string {
	return filepath.Join(p.metadataDir(refspec), "config")
}

func (p *Pool) layerInfoFile(refspec reference.Spec, dgst digest.Digest) string {
	return filepath.Join(p.metadataDir(refspec), colon2dash(dgst.String()))
}

func (p *Pool) loadManifestAndConfig(ctx context.Context, refspec reference.Spec) (manifest ocispec.Manifest, mPath string, config ocispec.Image, cPath string, err error) {
	manifest, mPath, config, cPath, err = p.readManifestAndConfig(refspec)
	if err == nil {
		log.G(ctx).Debugf("reusing manifest and config of %q", refspec.String())
		return
	}
	log.G(ctx).WithError(err).Debugf("fetching manifest and config of %q", refspec.String())
	manifest, config, err = fetchManifestAndConfig(ctx, p.hosts, refspec)
	if err != nil {
		return ocispec.Manifest{}, "", ocispec.Image{}, "", err
	}
	mPath, cPath, err = p.writeManifestAndConfig(refspec, manifest, config)
	if err != nil {
		return ocispec.Manifest{}, "", ocispec.Image{}, "", err
	}
	return manifest, mPath, config, cPath, err
}

func (p *Pool) loadLayerInfo(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (layerInfoPath string, err error) {
	layerInfoPath = p.layerInfoFile(refspec, dgst)
	if _, err := os.Stat(layerInfoPath); err == nil {
		log.G(ctx).Debugf("reusing layer info of %q/%q: %q",
			refspec.String(), dgst.String(), layerInfoPath)
		return layerInfoPath, nil
	}
	manifest, _, config, _, err := p.loadManifestAndConfig(ctx, refspec)
	if err != nil {
		return "", errors.Wrapf(err, "failed to get manifest and config")
	}
	info, err := genLayerInfo(ctx, dgst, manifest, config)
	if err != nil {
		return "", errors.Wrapf(err, "failed to generate layer info")
	}
	if err := os.MkdirAll(filepath.Dir(layerInfoPath), 0700); err != nil {
		return "", err
	}
	infoF, err := os.Create(layerInfoPath) // TODO: file mode
	if err != nil {
		return "", err
	}
	defer infoF.Close()
	return layerInfoPath, json.NewEncoder(infoF).Encode(&info)
}

func (p *Pool) loadLayer(ctx context.Context, refspec reference.Spec, target ocispec.Descriptor, preResolve []ocispec.Descriptor) (layer.Layer, error) {
	var (
		result     layer.Layer
		resultChan = make(chan layer.Layer)
		errChan    = make(chan error)
	)

	for _, l := range append([]ocispec.Descriptor{target}, preResolve...) {
		l := l

		// Check if layer is already resolved before creating goroutine.
		key := refspec.String() + "/" + l.Digest.String()
		p.layerMu.Lock()
		gotL, ok := p.layer[key]
		p.layerMu.Unlock()
		if ok {
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
			gotL, err := p.resolveLayer(ctx, refspec, l)
			if l.Digest.String() != target.Digest.String() {
				return // This is not target layer
			}
			if err != nil {
				errChan <- errors.Wrapf(err, "failed to resolve layer %q / %q",
					refspec, l.Digest)
				return
			}
			// Log this as preparation success
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareSucceeded).
				Debugf("successfully resolved layer")
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

func (p *Pool) resolveLayer(ctx context.Context, refspec reference.Spec, target ocispec.Descriptor) (layer.Layer, error) {
	key := refspec.String() + "/" + target.Digest.String()

	// Wait if resolving this layer is already running.
	p.resolveLock.Lock(key)
	defer p.resolveLock.Unlock(key)

	p.layerMu.Lock()
	gotL, ok := p.layer[key]
	p.layerMu.Unlock()
	if ok {
		// layer already resolved
		return gotL, nil
	}

	// Resolve this layer.
	l, err := p.resolver.Resolve(ctx, p.hosts, refspec, target)
	if err != nil {
		return nil, err
	}

	// Verify layer's content
	labels := target.Annotations
	if labels == nil {
		labels = make(map[string]string)
	}
	if p.disableVerification {
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
	if !p.noprefetch {
		go func() {
			p.backgroundTaskManager.DoPrioritizedTask()
			defer p.backgroundTaskManager.DonePrioritizedTask()
			if err := l.Prefetch(p.prefetchSize); err != nil {
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
	if !p.noBackgroundFetch {
		go func() {
			if err := l.BackgroundFetch(); err != nil {
				log.G(ctx).WithError(err).Debug("failed to fetch whole layer")
				return
			}
			log.G(ctx).Debug("completed to fetch all layer data in background")
		}()
	}

	// Register this layer.
	p.layerMu.Lock()
	p.layer[key] = l
	p.layerMu.Unlock()
	p.metricsController.Add(key, l)

	return l, nil
}

func (p *Pool) release(ref reference.Spec, dgst digest.Digest) (int, error) {
	// TODO: implement GC
	targetRef := ref.String()
	targetDgst := dgst.String()
	p.refcounterMu.Lock()
	defer p.refcounterMu.Unlock()
	if _, ok := p.refcounter[targetRef]; !ok {
		return 0, fmt.Errorf("ref %q not found during release", targetRef)
	}
	if c, ok := p.refcounter[targetRef][targetDgst]; !ok {
		return 0, fmt.Errorf("layer %q/%q not found during release", targetRef, targetDgst)
	} else if c <= 0 {
		return 0, fmt.Errorf("layer %q/%q isn't used", targetRef, targetDgst)
	}
	p.refcounter[targetRef][targetDgst]--
	return p.refcounter[targetRef][targetDgst], nil
}

func (p *Pool) use(ref reference.Spec, dgst digest.Digest) int {
	// TODO: implement GC
	targetRef := ref.String()
	targetDgst := dgst.String()
	p.refcounterMu.Lock()
	defer p.refcounterMu.Unlock()
	if _, ok := p.refcounter[targetRef]; !ok {
		p.refcounter[targetRef] = make(map[string]int)
	}
	p.refcounter[targetRef][targetDgst]++
	return p.refcounter[targetRef][targetDgst]
}

func (p *Pool) readManifestAndConfig(refspec reference.Spec) (manifest ocispec.Manifest, mPath string, config ocispec.Image, cPath string, _ error) {
	mPath, cPath = p.manifestFile(refspec), p.configFile(refspec)
	mf, err := os.Open(mPath)
	if err != nil {
		return ocispec.Manifest{}, "", ocispec.Image{}, "", err
	}
	defer mf.Close()
	if err := json.NewDecoder(mf).Decode(&manifest); err != nil {
		return ocispec.Manifest{}, "", ocispec.Image{}, "", err
	}
	cf, err := os.Open(cPath)
	if err != nil {
		return ocispec.Manifest{}, "", ocispec.Image{}, "", err
	}
	defer cf.Close()
	if err := json.NewDecoder(cf).Decode(&config); err != nil {
		return ocispec.Manifest{}, "", ocispec.Image{}, "", err
	}
	return manifest, mPath, config, cPath, nil
}

func (p *Pool) writeManifestAndConfig(refspec reference.Spec, manifest ocispec.Manifest, config ocispec.Image) (mPath string, cPath string, _ error) {
	mPath, cPath = p.manifestFile(refspec), p.configFile(refspec)
	if err := os.MkdirAll(filepath.Dir(mPath), 0700); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(cPath), 0700); err != nil {
		return "", "", err
	}
	mf, err := os.Create(mPath) // TODO: file mode
	if err != nil {
		return "", "", err
	}
	defer mf.Close()
	if err := json.NewEncoder(mf).Encode(&manifest); err != nil {
		return "", "", err
	}
	cf, err := os.Create(cPath) // TODO: file mode
	if err != nil {
		return "", "", err
	}
	defer cf.Close()
	if err := json.NewEncoder(cf).Encode(&config); err != nil {
		return "", "", err
	}
	return mPath, cPath, nil
}

func fetchManifestAndConfig(ctx context.Context, hosts source.RegistryHosts, refspec reference.Spec) (ocispec.Manifest, ocispec.Image, error) {
	// temporary resolver. should only be used for resolving `refpec`.
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: func(host string) ([]docker.RegistryHost, error) {
			if host != refspec.Hostname() {
				return nil, fmt.Errorf("unexpected host %q for image ref %q", host, refspec.String())
			}
			return hosts(refspec)
		},
	})
	_, img, err := resolver.Resolve(ctx, refspec.String())
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	fetcher, err := resolver.Fetcher(ctx, refspec.String())
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	plt := platforms.DefaultSpec() // TODO: should we make this configurable?
	manifest, err := fetchManifestPlatform(ctx, fetcher, img, plt)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	r, err := fetcher.Fetch(ctx, manifest.Config)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	defer r.Close()
	var config ocispec.Image
	if err := json.NewDecoder(r).Decode(&config); err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}

	return manifest, config, nil
}

func fetchManifestPlatform(ctx context.Context, fetcher remotes.Fetcher, desc ocispec.Descriptor, platform ocispec.Platform) (ocispec.Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	r, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Manifest{}, err
	}
	defer r.Close()

	var manifest ocispec.Manifest
	switch desc.MediaType {
	case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		err = json.NewDecoder(r).Decode(&manifest)
	case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
		var index ocispec.Index
		if err = json.NewDecoder(r).Decode(&index); err != nil {
			return ocispec.Manifest{}, err
		}
		var target ocispec.Descriptor
		found := false
		for _, m := range index.Manifests {
			p := platforms.DefaultSpec()
			if m.Platform != nil {
				p = *m.Platform
			}
			if !platforms.NewMatcher(platform).Match(p) {
				continue
			}
			target = m
			found = true
			break
		}
		if !found {
			return ocispec.Manifest{}, fmt.Errorf("no manifest found for platform")
		}
		manifest, err = fetchManifestPlatform(ctx, fetcher, target, platform)
	default:
		err = fmt.Errorf("unknown mediatype %q", desc.MediaType)
	}
	return manifest, err
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
