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

package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/labels"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	TargetRefLabel          = "containerd.io/snapshot/remote/stargz.reference"
	TargetDigestLabel       = "containerd.io/snapshot/remote/stargz.digest"
	TargetImageLayersLabel  = "containerd.io/snapshot/remote/stargz.layers"
	TargetPrefetchSizeLabel = "containerd.io/snapshot/remote/stargz.prefetch" // optional
)

// AppendInfoHandlerWrapper makes a handler which appends image's basic
// information to each layer descriptor as annotations during unpack. These
// annotations will be passed to this remote snapshotter as labels and used by
// this filesystem for searching/preparing layers.
func AppendInfoHandlerWrapper(ref string, prefetchSize int64) func(f images.Handler) images.Handler {
	return func(f images.Handler) images.Handler {
		return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			children, err := f.Handle(ctx, desc)
			if err != nil {
				return nil, err
			}
			switch desc.MediaType {
			case ocispec.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
				for i := range children {
					c := &children[i]
					if images.IsLayerType(c.MediaType) {
						if c.Annotations == nil {
							c.Annotations = make(map[string]string)
						}
						c.Annotations[TargetRefLabel] = ref
						c.Annotations[TargetDigestLabel] = c.Digest.String()
						var layers string
						for _, l := range children[i:] {
							if images.IsLayerType(l.MediaType) {
								ls := fmt.Sprintf("%s,", l.Digest.String())
								// This avoids the label hits the size limitation.
								// Skipping layers is allowed here and only affects performance.
								if err := labels.Validate(TargetImageLayersLabel, layers+ls); err != nil {
									break
								}
								layers += ls
							}
						}
						c.Annotations[TargetImageLayersLabel] = strings.TrimSuffix(layers, ",")
						c.Annotations[TargetPrefetchSizeLabel] = fmt.Sprintf("%d", prefetchSize)
					}
				}
			}
			return children, nil
		})
	}
}
