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

package ipfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/platforms"
	"github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-files"
	iface "github.com/ipfs/interface-go-ipfs-core"
	"github.com/ipfs/interface-go-ipfs-core/options"
	ipath "github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Push pushes the provided image ref to IPFS with converting it to IPFS-enabled format.
// Use Resovler to fetch that image.
func Push(ctx context.Context, client *containerd.Client, api iface.CoreAPI, ref string, layerConvert converter.ConvertFunc, platformMC platforms.MatchComparer) (ipath.Resolved, error) {
	ctx, done, err := client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)
	srcImg, err := client.ImageService().Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	dstDesc, err := IndexConvertFunc(api, layerConvert, platformMC)(ctx, client.ContentStore(), srcImg.Target)
	if err != nil {
		return nil, err
	}
	root, err := json.Marshal(dstDesc)
	if err != nil {
		return nil, err
	}
	return api.Unixfs().Add(ctx, files.NewBytesFile(root), options.Unixfs.Pin(true), options.Unixfs.CidVersion(1))
}

// IndexConvertFunc converts an image and add it to the IPFS.
func IndexConvertFunc(api iface.CoreAPI, layerConvertFunc converter.ConvertFunc, platformMC platforms.MatchComparer) converter.ConvertFunc {
	c := &defaultConverter{
		api:              api,
		layerConvertFunc: layerConvertFunc,
		docker2oci:       true, // TODO: should it be configurable?
		platformMC:       platformMC,
		diffIDMap:        make(map[digest.Digest]digest.Digest),
	}
	return c.convert
}

type defaultConverter struct {
	api              iface.CoreAPI
	layerConvertFunc converter.ConvertFunc
	docker2oci       bool
	platformMC       platforms.MatchComparer
	diffIDMap        map[digest.Digest]digest.Digest // key: old diffID, value: new diffID
	diffIDMapMu      sync.RWMutex
}

// convert dispatches desc.MediaType and calls c.convert{Layer,Manifest,Index,Config}.
//
// Also converts media type if c.docker2oci is set.
func (c *defaultConverter) convert(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		newDesc *ocispec.Descriptor
		err     error
	)
	if images.IsLayerType(desc.MediaType) {
		newDesc, err = c.convertLayer(ctx, cs, desc)
	} else if images.IsManifestType(desc.MediaType) {
		newDesc, err = c.convertManifest(ctx, cs, desc)
	} else if images.IsIndexType(desc.MediaType) {
		newDesc, err = c.convertIndex(ctx, cs, desc)
	} else if images.IsConfigType(desc.MediaType) {
		newDesc, err = c.convertConfig(ctx, cs, desc)
	}
	if err != nil {
		return nil, err
	}
	if images.IsDockerType(desc.MediaType) {
		if c.docker2oci {
			if newDesc == nil {
				newDesc = copyDesc(desc)
			}
			newDesc.MediaType = converter.ConvertDockerMediaTypeToOCI(newDesc.MediaType)
		} else if (newDesc == nil && len(desc.Annotations) != 0) || (newDesc != nil && len(newDesc.Annotations) != 0) {
			// Annotations is supported only on OCI manifest.
			// We need to remove annotations for Docker media types.
			if newDesc == nil {
				newDesc = copyDesc(desc)
			}
			newDesc.Annotations = nil
		}
	}

	// TODO: patch upstream to allow this as a callback hook
	if newDesc == nil {
		newDesc = copyDesc(desc)
	}
	ra, err := cs.ReaderAt(ctx, *newDesc)
	if err != nil {
		return nil, err
	}
	p, err := c.api.Unixfs().Add(ctx, files.NewReaderFile(content.NewReader(ra)), options.Unixfs.Pin(true), options.Unixfs.CidVersion(1))
	if err != nil {
		return nil, err
	}
	// record IPFS URL using CIDv1 : https://docs.ipfs.io/how-to/address-ipfs-on-web/#native-urls
	if p.Cid().Version() == 0 {
		return nil, fmt.Errorf("CID verions 0 isn't supported")
	}
	newDesc.URLs = []string{"ipfs://" + p.Cid().String()}

	logrus.WithField("old", desc).WithField("new", newDesc).Debugf("converted")
	return newDesc, nil
}

func GetPath(desc ocispec.Descriptor) (ipath.Path, error) {
	for _, u := range desc.URLs {
		if strings.HasPrefix(u, "ipfs://") {
			// support only content addressable URL (ipfs://<CID>)
			c, err := cid.Decode(u[7:])
			if err != nil {
				return nil, err
			}
			return ipath.IpfsPath(c), nil
		}
	}
	return nil, fmt.Errorf("no CID is recorded")
}

func copyDesc(desc ocispec.Descriptor) *ocispec.Descriptor {
	descCopy := desc
	return &descCopy
}

// convertLayer converts image image layers if c.layerConvertFunc is set.
//
// c.layerConvertFunc can be nil, e.g., for converting Docker media types to OCI ones.
func (c *defaultConverter) convertLayer(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	if c.layerConvertFunc != nil {
		return c.layerConvertFunc(ctx, cs, desc)
	}
	return nil, nil
}

// convertManifest converts image manifests.
//
// - clears `.mediaType` if the target format is OCI
//
// - records diff ID changes in c.diffIDMap
func (c *defaultConverter) convertManifest(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		manifest DualManifest
		modified bool
	)
	labels, err := readJSON(ctx, cs, &manifest, desc)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	if images.IsDockerType(manifest.MediaType) && c.docker2oci {
		manifest.MediaType = ""
		modified = true
	}
	var mu sync.Mutex
	eg, ctx2 := errgroup.WithContext(ctx)
	for i, l := range manifest.Layers {
		i := i
		l := l
		oldDiffID, err := images.GetDiffID(ctx, cs, l)
		if err != nil {
			return nil, err
		}
		eg.Go(func() error {
			newL, err := c.convert(ctx2, cs, l)
			if err != nil {
				return err
			}
			if newL != nil {
				mu.Lock()
				// update GC labels
				converter.ClearGCLabels(labels, l.Digest)
				labelKey := fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)
				labels[labelKey] = newL.Digest.String()
				manifest.Layers[i] = *newL
				modified = true
				mu.Unlock()

				// diffID changes if the tar entries were modified.
				// diffID stays same if only the compression type was changed.
				// When diffID changed, add a map entry so that we can update image config.
				newDiffID, err := images.GetDiffID(ctx, cs, *newL)
				if err != nil {
					return err
				}
				if newDiffID != oldDiffID {
					c.diffIDMapMu.Lock()
					c.diffIDMap[oldDiffID] = newDiffID
					c.diffIDMapMu.Unlock()
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	newConfig, err := c.convert(ctx, cs, manifest.Config)
	if err != nil {
		return nil, err
	}
	if newConfig != nil {
		converter.ClearGCLabels(labels, manifest.Config.Digest)
		labels["containerd.io/gc.ref.content.config"] = newConfig.Digest.String()
		manifest.Config = *newConfig
		modified = true
	}

	if modified {
		return writeJSON(ctx, cs, &manifest, desc, labels)
	}
	return nil, nil
}

// convertIndex converts image index.
//
// - clears `.mediaType` if the target format is OCI
//
// - clears manifest entries that do not match c.platformMC
func (c *defaultConverter) convertIndex(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		index    DualIndex
		modified bool
	)
	labels, err := readJSON(ctx, cs, &index, desc)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	if images.IsDockerType(index.MediaType) && c.docker2oci {
		index.MediaType = ""
		modified = true
	}

	newManifests := make([]ocispec.Descriptor, len(index.Manifests))
	newManifestsToBeRemoved := make(map[int]struct{}) // slice index
	var mu sync.Mutex
	eg, ctx2 := errgroup.WithContext(ctx)
	for i, mani := range index.Manifests {
		i := i
		mani := mani
		labelKey := fmt.Sprintf("containerd.io/gc.ref.content.m.%d", i)
		eg.Go(func() error {
			if mani.Platform != nil && !c.platformMC.Match(*mani.Platform) {
				mu.Lock()
				converter.ClearGCLabels(labels, mani.Digest)
				newManifestsToBeRemoved[i] = struct{}{}
				modified = true
				mu.Unlock()
				return nil
			}
			newMani, err := c.convert(ctx2, cs, mani)
			if err != nil {
				return err
			}
			mu.Lock()
			if newMani != nil {
				converter.ClearGCLabels(labels, mani.Digest)
				labels[labelKey] = newMani.Digest.String()
				// NOTE: for keeping manifest order, we specify `i` index explicitly
				newManifests[i] = *newMani
				modified = true
			} else {
				newManifests[i] = mani
			}
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	if modified {
		var newManifestsClean []ocispec.Descriptor
		for i, m := range newManifests {
			if _, ok := newManifestsToBeRemoved[i]; !ok {
				newManifestsClean = append(newManifestsClean, m)
			}
		}
		index.Manifests = newManifestsClean
		return writeJSON(ctx, cs, &index, desc, labels)
	}
	return nil, nil
}

// convertConfig converts image config contents.
//
// - updates `.rootfs.diff_ids` using c.diffIDMap .
//
// - clears legacy `.config.Image` and `.container_config.Image` fields if `.rootfs.diff_ids` was updated.
func (c *defaultConverter) convertConfig(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		cfg      DualConfig
		cfgAsOCI ocispec.Image // read only, used for parsing cfg
		modified bool
	)

	labels, err := readJSON(ctx, cs, &cfg, desc)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	if _, err := readJSON(ctx, cs, &cfgAsOCI, desc); err != nil {
		return nil, err
	}

	if rootfs := cfgAsOCI.RootFS; rootfs.Type == "layers" {
		rootfsModified := false
		c.diffIDMapMu.RLock()
		for i, oldDiffID := range rootfs.DiffIDs {
			if newDiffID, ok := c.diffIDMap[oldDiffID]; ok && newDiffID != oldDiffID {
				rootfs.DiffIDs[i] = newDiffID
				rootfsModified = true
			}
		}
		c.diffIDMapMu.RUnlock()
		if rootfsModified {
			rootfsB, err := json.Marshal(rootfs)
			if err != nil {
				return nil, err
			}
			cfg["rootfs"] = (*json.RawMessage)(&rootfsB)
			modified = true
		}
	}

	if modified {
		// cfg may have dummy value for legacy `.config.Image` and `.container_config.Image`
		// We should clear the ID if we changed the diff IDs.
		if _, err := clearDockerV1DummyID(cfg); err != nil {
			return nil, err
		}
		return writeJSON(ctx, cs, &cfg, desc, labels)
	}
	return nil, nil
}

// clearDockerV1DummyID clears the dummy values for legacy `.config.Image` and `.container_config.Image`.
// Returns true if the cfg was modified.
func clearDockerV1DummyID(cfg DualConfig) (bool, error) {
	var modified bool
	f := func(k string) error {
		if configX, ok := cfg[k]; ok && configX != nil {
			var configField map[string]*json.RawMessage
			if err := json.Unmarshal(*configX, &configField); err != nil {
				return err
			}
			delete(configField, "Image")
			b, err := json.Marshal(configField)
			if err != nil {
				return err
			}
			cfg[k] = (*json.RawMessage)(&b)
			modified = true
		}
		return nil
	}
	if err := f("config"); err != nil {
		return modified, err
	}
	if err := f("container_config"); err != nil {
		return modified, err
	}
	return modified, nil
}

// ObjectWithMediaType represents an object with a MediaType field
type ObjectWithMediaType struct {
	// MediaType appears on Docker manifests and manifest lists.
	// MediaType does not appear on OCI manifests and index
	MediaType string `json:"mediaType,omitempty"`
}

// DualManifest covers Docker manifest and OCI manifest
type DualManifest struct {
	ocispec.Manifest
	ObjectWithMediaType
}

// DualIndex covers Docker manifest list and OCI index
type DualIndex struct {
	ocispec.Index
	ObjectWithMediaType
}

// DualConfig covers Docker config (v1.0, v1.1, v1.2) and OCI config.
// Unmarshalled as map[string]*json.RawMessage to retain unknown fields on remarshalling.
type DualConfig map[string]*json.RawMessage

func readJSON(ctx context.Context, cs content.Store, x interface{}, desc ocispec.Descriptor) (map[string]string, error) {
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil {
		return nil, err
	}
	labels := info.Labels
	b, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, x); err != nil {
		return nil, err
	}
	return labels, nil
}

func writeJSON(ctx context.Context, cs content.Store, x interface{}, oldDesc ocispec.Descriptor, labels map[string]string) (*ocispec.Descriptor, error) {
	b, err := json.Marshal(x)
	if err != nil {
		return nil, err
	}
	dgst := digest.SHA256.FromBytes(b)
	ref := fmt.Sprintf("converter-write-json-%s", dgst.String())
	w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
	if err != nil {
		return nil, err
	}
	if err := content.Copy(ctx, w, bytes.NewReader(b), int64(len(b)), dgst, content.WithLabels(labels)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	newDesc := oldDesc
	newDesc.Size = int64(len(b))
	newDesc.Digest = dgst
	return &newDesc, nil
}
