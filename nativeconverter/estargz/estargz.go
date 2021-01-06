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

package estargz

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/nativeconverter"
	"github.com/containerd/stargz-snapshotter/nativeconverter/uncompress"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// LayerConvertWithOptsFunc converts legacy tar.gz layers into eStargz tar.gz layers.
// Media type is unchanged. Should be used in conjunction with WithDockerToOCI(). See
// LayerConvertFunc for more details. The difference between this function and
// LayerConvertFunc is that this allows to specify additional eStargz options per layer.
func LayerConvertWithOptsFunc(commonOpts []estargz.Option, opts map[digest.Digest][]estargz.Option) nativeconverter.ConvertFunc {
	if opts == nil {
		return LayerConvertFunc(commonOpts...)
	}
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		return LayerConvertFunc(append(commonOpts, opts[desc.Digest]...)...)(ctx, cs, desc)
	}
}

// LayerConvertFunc converts legacy tar.gz layers into eStargz tar.gz layers.
// Media type is unchanged.
//
// Should be used in conjunction with WithDockerToOCI().
//
// Otherwise "containerd.io/snapshot/stargz/toc.digest" annotation will be lost,
// because the Docker media type does not support layer annotations.
func LayerConvertFunc(opts ...estargz.Option) nativeconverter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		if !images.IsLayerType(desc.MediaType) {
			// No conversion. No need to return an error here.
			return nil, nil
		}
		uncompressedDesc := &desc
		// We need to uncompress the archive first to call estargz.Build.
		if !uncompress.IsUncompressedType(desc.MediaType) {
			var err error
			uncompressedDesc, err = uncompress.LayerConvertFunc(ctx, cs, desc)
			if err != nil {
				return nil, err
			}
			if uncompressedDesc == nil {
				return nil, errors.Errorf("unexpectedly got the same blob aftr compression (%s, %q)", desc.Digest, desc.MediaType)
			}
			defer func() {
				if err := cs.Delete(ctx, uncompressedDesc.Digest); err != nil {
					logrus.WithError(err).WithField("uncompressedDesc", uncompressedDesc).Warn("failed to remove tmp uncompressed layer")
				}
			}()
			logrus.Debugf("estargz: uncompressed %s into %s", desc.Digest, uncompressedDesc.Digest)
		}

		info, err := cs.Info(ctx, desc.Digest)
		if err != nil {
			return nil, err
		}
		labels := info.Labels

		uncompressedReaderAt, err := cs.ReaderAt(ctx, *uncompressedDesc)
		if err != nil {
			return nil, err
		}
		defer uncompressedReaderAt.Close()
		uncompressedSR := io.NewSectionReader(uncompressedReaderAt, 0, uncompressedDesc.Size)
		blob, err := estargz.Build(uncompressedSR, opts...)
		if err != nil {
			return nil, err
		}
		defer blob.Close()
		ref := fmt.Sprintf("convert-estargz-from-%s", desc.Digest)
		w, err := cs.Writer(ctx, content.WithRef(ref))
		if err != nil {
			return nil, err
		}
		defer w.Close()
		n, err := io.Copy(w, blob)
		if err != nil {
			return nil, err
		}
		if err := blob.Close(); err != nil {
			return nil, err
		}
		// update diffID label
		labels[nativeconverter.LabelUncompressed] = blob.DiffID().String()
		if err = w.Commit(ctx, n, "", content.WithLabels(labels)); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		newDesc := desc
		if uncompress.IsUncompressedType(newDesc.MediaType) {
			if nativeconverter.IsDockerType(newDesc.MediaType) {
				newDesc.MediaType += ".gzip"
			} else {
				newDesc.MediaType += "+gzip"
			}
		}
		newDesc.Digest = w.Digest()
		newDesc.Size = n
		if newDesc.Annotations == nil {
			newDesc.Annotations = make(map[string]string, 1)
		}
		newDesc.Annotations[estargz.TOCJSONDigestAnnotation] = blob.TOCDigest().String()
		return &newDesc, nil
	}
}
