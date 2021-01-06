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

package uncompress

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/stargz-snapshotter/nativeconverter"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerConvertFunc converts tar.gz layers into uncompressed tar layers.
// Media type is changed, e.g., "application/vnd.oci.image.layer.v1.tar+gzip" -> "application/vnd.oci.image.layer.v1.tar"
func LayerConvertFunc(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	if !images.IsLayerType(desc.MediaType) || IsUncompressedType(desc.MediaType) {
		// No conversion. No need to return an error here.
		return nil, nil
	}
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil {
		return nil, err
	}
	// Check if uncompressed content already exists in the content store
	if uDgstStr, ok := info.Labels[nativeconverter.LabelUncompressed]; ok {
		if uDgst, err := digest.Parse(uDgstStr); err == nil {
			uInfo, err := cs.Info(ctx, uDgst)
			if err == nil {
				newDesc := desc
				newDesc.Digest = uInfo.Digest
				newDesc.Size = uInfo.Size
				newDesc.MediaType = convertMediaType(newDesc.MediaType)
				return &newDesc, nil
			}
		}
	}
	readerAt, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer readerAt.Close()
	sr := io.NewSectionReader(readerAt, 0, desc.Size)
	newR, err := gzip.NewReader(sr)
	if err != nil {
		return nil, err
	}
	defer newR.Close()
	ref := fmt.Sprintf("convert-uncompress-from-%s", desc.Digest)
	w, err := cs.Writer(ctx, content.WithRef(ref))
	if err != nil {
		return nil, err
	}
	defer w.Close()
	n, err := io.Copy(w, newR)
	if err != nil {
		return nil, err
	}
	if err := newR.Close(); err != nil {
		return nil, err
	}
	// no need to retain "containerd.io/uncompressed" label, but retain other labels ("containerd.io/distribution.source.*")
	labels := info.Labels
	delete(labels, nativeconverter.LabelUncompressed)
	if err = w.Commit(ctx, 0, "", content.WithLabels(labels)); err != nil && !errdefs.IsAlreadyExists(err) {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	newDesc := desc
	newDesc.Digest = w.Digest()
	newDesc.Size = n
	newDesc.MediaType = convertMediaType(newDesc.MediaType)
	return &newDesc, nil
}

func IsUncompressedType(mt string) bool {
	switch mt {
	case
		images.MediaTypeDockerSchema2Layer,
		images.MediaTypeDockerSchema2LayerForeign,
		ocispec.MediaTypeImageLayer,
		ocispec.MediaTypeImageLayerNonDistributable:
		return true
	default:
		return false
	}
}

func convertMediaType(mt string) string {
	switch mt {
	case images.MediaTypeDockerSchema2LayerGzip:
		return images.MediaTypeDockerSchema2Layer
	case images.MediaTypeDockerSchema2LayerForeignGzip:
		return images.MediaTypeDockerSchema2LayerForeign
	case ocispec.MediaTypeImageLayerGzip:
		return ocispec.MediaTypeImageLayer
	case ocispec.MediaTypeImageLayerNonDistributableGzip:
		return ocispec.MediaTypeImageLayerNonDistributable
	default:
		return mt
	}
}
