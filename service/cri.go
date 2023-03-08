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

package service

import (
	"fmt"
	"strings"

	"github.com/containerd/containerd/reference"
	"github.com/containerd/stargz-snapshotter/fs/source"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TODO: switch to "github.com/containerd/containerd/pkg/snapshotters" once all tools using
//
//	stargz-snapshotter (e.g. k3s) move to containerd version where that pkg is available.
const (
	// targetRefLabel is a label which contains image reference and will be passed
	// to snapshotters.
	targetRefLabel = "containerd.io/snapshot/cri.image-ref"
	// targetLayerDigestLabel is a label which contains layer digest and will be passed
	// to snapshotters.
	targetLayerDigestLabel = "containerd.io/snapshot/cri.layer-digest"
	// targetImageLayersLabel is a label which contains layer digests contained in
	// the target image and will be passed to snapshotters for preparing layers in
	// parallel. Skipping some layers is allowed and only affects performance.
	targetImageLayersLabel = "containerd.io/snapshot/cri.image-layers"
)

const (
	// targetImageURLsLabelPrefix is a label prefix which constructs a map from the layer index to
	// urls of the layer descriptor. This isn't contained in the set of the labels passed from CRI plugin but
	// some clients (e.g. nerdctl) passes this for preserving url field in the OCI descriptor.
	targetImageURLsLabelPrefix = "containerd.io/snapshot/remote/urls."

	// targetURsLLabel is a label which contains layer URL. This is only used to pass URL from containerd
	// to snapshotter. This isn't contained in the set of the labels passed from CRI plugin but
	// some clients (e.g. nerdctl) passes this for preserving url field in the OCI descriptor.
	targetURLsLabel = "containerd.io/snapshot/remote/urls"
)

func sourceFromCRILabels(hosts source.RegistryHosts) source.GetSources {
	return func(labels map[string]string) ([]source.Source, error) {
		refStr, ok := labels[targetRefLabel]
		if !ok {
			return nil, fmt.Errorf("reference hasn't been passed")
		}
		refspec, err := reference.Parse(refStr)
		if err != nil {
			return nil, err
		}

		digestStr, ok := labels[targetLayerDigestLabel]
		if !ok {
			return nil, fmt.Errorf("digest hasn't been passed")
		}
		target, err := digest.Parse(digestStr)
		if err != nil {
			return nil, err
		}

		var neighboringLayers []ocispec.Descriptor
		if l, ok := labels[targetImageLayersLabel]; ok {
			layersStr := strings.Split(l, ",")
			for i, l := range layersStr {
				d, err := digest.Parse(l)
				if err != nil {
					return nil, err
				}
				if d.String() != target.String() {
					desc := ocispec.Descriptor{Digest: d}
					if urls, ok := labels[targetImageURLsLabelPrefix+fmt.Sprintf("%d", i)]; ok {
						desc.URLs = strings.Split(urls, ",")
					}
					neighboringLayers = append(neighboringLayers, desc)
				}
			}
		}

		targetDesc := ocispec.Descriptor{
			Digest:      target,
			Annotations: labels,
		}
		if targetURLs, ok := labels[targetURLsLabel]; ok {
			targetDesc.URLs = append(targetDesc.URLs, strings.Split(targetURLs, ",")...)
		}

		return []source.Source{
			{
				Hosts:    hosts,
				Name:     refspec,
				Target:   targetDesc,
				Manifest: ocispec.Manifest{Layers: append([]ocispec.Descriptor{targetDesc}, neighboringLayers...)},
			},
		}, nil
	}
}
