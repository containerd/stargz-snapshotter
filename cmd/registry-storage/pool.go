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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/stargz-snapshotter/estargz"
	stargzfs "github.com/containerd/stargz-snapshotter/fs"
	"github.com/containerd/stargz-snapshotter/snapshot/types"
	"github.com/containers/storage/pkg/archive"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	// targetRefLabel is a label which contains image reference.
	targetRefLabel = "containerd.io/snapshot/remote/stargz.reference"

	// targetDigestLabel is a label which contains layer digest.
	targetDigestLabel = "containerd.io/snapshot/remote/stargz.digest"

	// targetImageLayersLabel is a label which contains layer digests contained in
	// the target image.
	targetImageLayersLabel = "containerd.io/snapshot/remote/stargz.layers"

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
)

type pool struct {
	fs           types.FileSystem
	path         string
	mounted      map[string]struct{}
	mountedMu    sync.Mutex
	hosts        docker.RegistryHosts
	refcounter   map[string]map[string]int
	refcounterMu sync.Mutex
}

func (p *pool) root() string {
	return p.path
}

func (p *pool) metadataDir(refspec reference.Spec) string {
	return filepath.Join(p.path,
		"metadata--"+colon2dash(digest.FromString(refspec.String()).String()))
}

func (p *pool) manifestFile(refspec reference.Spec) string {
	return filepath.Join(p.metadataDir(refspec), "manifest")
}

func (p *pool) configFile(refspec reference.Spec) string {
	return filepath.Join(p.metadataDir(refspec), "config")
}

func (p *pool) layerInfoFile(refspec reference.Spec, dgst digest.Digest) string {
	return filepath.Join(p.metadataDir(refspec), colon2dash(dgst.String()))
}

func (p *pool) diffDir(refspec reference.Spec, dgst digest.Digest) string {
	return filepath.Join(p.path,
		"diffs--"+colon2dash(digest.FromString(refspec.String()).String()),
		colon2dash(dgst.String()),
	)
}

func (p *pool) chainDir(refspec reference.Spec, digestChainID digest.Digest) string {
	return filepath.Join(p.path,
		"chain--"+colon2dash(
			digest.FromString(refspec.String()+" "+digestChainID.String()).String()))
}

func (p *pool) loadManifestAndConfig(ctx context.Context, refspec reference.Spec) (manifest ocispec.Manifest, mPath string, config ocispec.Image, cPath string, err error) {
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

func (p *pool) loadLayerInfo(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (layerInfoPath string, err error) {
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
	info, err := genLayerInfo(dgst, manifest, config)
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

func (p *pool) loadChain(ctx context.Context, refspec reference.Spec, chain []ocispec.Descriptor) (string, error) {
	if len(chain) == 0 {
		return "", fmt.Errorf("chain is nil")
	}

	digestChainID := chain[0].Digest // is not OCI chainID that consists of DiffIDs
	for i := 1; i < len(chain); i++ {
		digestChainID = digest.SHA256.FromString(fmt.Sprintf("%s %s", digestChainID, chain[i].Digest))
	}
	mountpoint := p.chainDir(refspec, digestChainID)
	log.G(ctx).Debugf("mounting at %q", mountpoint)

	p.mountedMu.Lock()
	if _, ok := p.mounted[mountpoint]; ok {
		p.mountedMu.Unlock()
		log.G(ctx).Debugf("already mounted at %q", mountpoint)
		return mountpoint, nil // alredy mounted // TODO: retry on unhealthy
	}
	p.mounted[mountpoint] = struct{}{}
	p.mountedMu.Unlock()

	if err := os.MkdirAll(mountpoint, 0700); err != nil {
		return "", err
	}

	commonLabels := make(map[string]string)
	var layers []string
	for _, l := range chain {
		if images.IsLayerType(l.MediaType) {
			layers = append(layers, l.Digest.String())
			if tocInfo, ok := l.Annotations[estargz.ZstdChunkedManifestInfoAnnotation]; ok {
				var off, compressedSize, uncompressedSize, manifestType uint64
				if _, err := fmt.Sscanf(tocInfo, "%d:%d:%d:%d",
					&off, &compressedSize, &uncompressedSize, &manifestType,
				); err == nil {
					commonLabels[stargzfs.TocOffsetLabelPrefix+l.Digest.String()] = fmt.Sprintf("%d", off)
				}
			}
		}
	}
	commonLabels[targetImageLayersLabel] = strings.Join(layers, ",")
	// TODO: unmount on failure
	chainPaths := make([]string, len(chain))
	for i, l := range chain {
		labels := make(map[string]string)
		for k, v := range commonLabels {
			labels[k] = v
		}
		if l.Annotations != nil {
			tocDigest, ok := l.Annotations[estargz.TOCJSONDigestAnnotation]
			if ok {
				labels[estargz.TOCJSONDigestAnnotation] = tocDigest
			}
		}
		mp, err := p.loadLayer(ctx, refspec, l.Digest, labels)
		if err != nil {
			return "", err
		}
		chainPaths[len(chain)-i-1] = mp // the first element is the topmost layer
	}

	var mountInfo mount.Mount
	if len(chain) == 1 {
		mountInfo = mount.Mount{
			Source: chainPaths[0],
			Type:   "bind",
			Options: []string{
				"ro",
				"rbind",
			},
		}
	} else {
		mountInfo = mount.Mount{
			Type:    "overlay",
			Source:  "overlay",
			Options: []string{fmt.Sprintf("lowerdir=%s", strings.Join(chainPaths, ":"))},
		}
	}

	return mountpoint, errors.Wrapf(mount.All([]mount.Mount{mountInfo}, mountpoint),
		"failed to mount at %q(%+v)", mountpoint, mountInfo)
}

func (p *pool) loadLayer(ctx context.Context, refspec reference.Spec, dgst digest.Digest, labels map[string]string) (string, error) {
	mp := p.diffDir(refspec, dgst)
	if err := os.MkdirAll(mp, 0700); err != nil {
		return "", err
	}
	p.mountedMu.Lock()
	if _, ok := p.mounted[mp]; ok {
		p.mountedMu.Unlock()
		return mp, nil // alredy mounted // TODO: retry on unhealthy
	}
	p.mounted[mp] = struct{}{}
	p.mountedMu.Unlock()

	if labels == nil {
		labels = make(map[string]string)
	}

	log.G(ctx).WithField("moutpoint", mp).
		WithField("reference", refspec.String()).
		WithField("digest", dgst.String()).Infof("mounting")
	labels[targetRefLabel] = refspec.String()
	labels[targetDigestLabel] = dgst.String()
	if err := p.fs.Mount(ctx, mp, labels); err != nil {
		if cErr := ctx.Err(); errors.Is(cErr, context.Canceled) || errors.Is(err, context.Canceled) {
			// When filesystem client canceled to lookup this layer, do not log
			// this as "preparation failure" because it's intensional.
			log.G(ctx).WithError(err).
				Debugf("canceled to mount layer (context error: %v)", cErr)
		} else {
			// Log this as preparation failure
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithError(err).Debugf("failed to mount layer (context error: %v)", cErr)
		}
		return "", errors.Wrapf(err, "failed to mount layer %q", dgst.String())
	}
	log.G(ctx).WithField(remoteSnapshotLogKey, prepareSucceeded).Debug("prepared remote snapshot")

	return mp, nil
}

func (p *pool) release(ref reference.Spec, dgst digest.Digest) (int, error) {
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

func (p *pool) use(ref reference.Spec, dgst digest.Digest) int {
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

func (p *pool) readManifestAndConfig(refspec reference.Spec) (manifest ocispec.Manifest, mPath string, config ocispec.Image, cPath string, _ error) {
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

func (p *pool) writeManifestAndConfig(refspec reference.Spec, manifest ocispec.Manifest, config ocispec.Image) (mPath string, cPath string, _ error) {
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

func fetchManifestAndConfig(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec) (ocispec.Manifest, ocispec.Image, error) {
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: hosts,
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
	CompressedDigest   digest.Digest       `json:"compressed-diff-digest,omitempty"`
	CompressedSize     int64               `json:"compressed-size,omitempty"`
	UncompressedDigest digest.Digest       `json:"diff-digest,omitempty"`
	UncompressedSize   int64               `json:"diff-size,omitempty"`
	CompressionType    archive.Compression `json:"compression,omitempty"`
	ReadOnly           bool                `json:"-"`
}

func genLayerInfo(dgst digest.Digest, manifest ocispec.Manifest, config ocispec.Image) (Layer, error) {
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
	return Layer{
		CompressedDigest:   manifest.Layers[layerIndex].Digest,
		CompressedSize:     manifest.Layers[layerIndex].Size,
		UncompressedDigest: config.RootFS.DiffIDs[layerIndex],
		UncompressedSize:   0, // TODO
		CompressionType:    archive.Gzip,
		ReadOnly:           true,
	}, nil
}
