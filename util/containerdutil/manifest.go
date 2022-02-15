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

package containerdutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func ManifestDesc(ctx context.Context, provider content.Provider, image ocispec.Descriptor, platform platforms.MatchComparer) (ocispec.Descriptor, error) {
	m, err := ManifestDescs(ctx, provider, image, platform)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return m[0], nil
}

func ManifestDescs(ctx context.Context, provider content.Provider, image ocispec.Descriptor, platform platforms.MatchComparer) ([]ocispec.Descriptor, error) {
	var (
		m        []ocispec.Descriptor
		wasIndex bool
	)
	if err := images.Walk(ctx, images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
			p, err := content.ReadBlob(ctx, provider, desc)
			if err != nil {
				return nil, err
			}
			if err := ValidateMediaType(p, desc.MediaType); err != nil {
				return nil, err
			}
			var manifest ocispec.Manifest
			if err := json.Unmarshal(p, &manifest); err != nil {
				return nil, err
			}
			if desc.Digest != image.Digest {
				if desc.Platform != nil && !platform.Match(*desc.Platform) {
					return nil, nil
				}
				if desc.Platform == nil {
					p, err := content.ReadBlob(ctx, provider, manifest.Config)
					if err != nil {
						return nil, err
					}
					var image ocispec.Image
					if err := json.Unmarshal(p, &image); err != nil {
						return nil, err
					}
					if !platform.Match(platforms.Normalize(ocispec.Platform{OS: image.OS, Architecture: image.Architecture})) {
						return nil, nil
					}
				}
			}
			m = append(m, desc)
			return nil, nil
		case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
			p, err := content.ReadBlob(ctx, provider, desc)
			if err != nil {
				return nil, err
			}
			if err := ValidateMediaType(p, desc.MediaType); err != nil {
				return nil, err
			}
			var idx ocispec.Index
			if err := json.Unmarshal(p, &idx); err != nil {
				return nil, err
			}
			var descs []ocispec.Descriptor
			for _, d := range idx.Manifests {
				if d.Platform == nil || platform.Match(*d.Platform) {
					descs = append(descs, d)
				}
			}
			sort.SliceStable(descs, func(i, j int) bool {
				if descs[i].Platform == nil {
					return false
				}
				if descs[j].Platform == nil {
					return true
				}
				return platform.Less(*descs[i].Platform, *descs[j].Platform)
			})
			wasIndex = true
			return descs, nil
		}
		return nil, fmt.Errorf("unexpected media type %v for %v: %w", desc.MediaType, desc.Digest, errdefs.ErrNotFound)
	}), image); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		err := fmt.Errorf("manifest %v: %w", image.Digest, errdefs.ErrNotFound)
		if wasIndex {
			err = fmt.Errorf("no match for platform in manifest %v: %w", image.Digest, errdefs.ErrNotFound)
		}
		return nil, err
	}
	return m, nil
}

// Forked from github.com/containerd/containerd/images/image.go
// commit: a776a27af54a803657d002e7574a4425b3949f56

// unknownDocument represents a manifest, manifest list, or index that has not
// yet been validated.
type unknownDocument struct {
	MediaType string          `json:"mediaType,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
	Layers    json.RawMessage `json:"layers,omitempty"`
	Manifests json.RawMessage `json:"manifests,omitempty"`
	FSLayers  json.RawMessage `json:"fsLayers,omitempty"` // schema 1
}

// ValidateMediaType returns an error if the byte slice is invalid JSON or if
// the media type identifies the blob as one format but it contains elements of
// another format.
func ValidateMediaType(b []byte, mt string) error {
	var doc unknownDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	if len(doc.FSLayers) != 0 {
		return fmt.Errorf("media-type: schema 1 not supported")
	}
	switch mt {
	case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		if len(doc.Manifests) != 0 ||
			doc.MediaType == images.MediaTypeDockerSchema2ManifestList ||
			doc.MediaType == ocispec.MediaTypeImageIndex {
			return fmt.Errorf("media-type: expected manifest but found index (%s)", mt)
		}
	case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
		if len(doc.Config) != 0 || len(doc.Layers) != 0 ||
			doc.MediaType == images.MediaTypeDockerSchema2Manifest ||
			doc.MediaType == ocispec.MediaTypeImageManifest {
			return fmt.Errorf("media-type: expected index but found manifest (%s)", mt)
		}
	}
	return nil
}
