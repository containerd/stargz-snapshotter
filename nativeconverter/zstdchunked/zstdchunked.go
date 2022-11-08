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

package zstdchunked

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/images/converter/uncompress"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/containerd/stargz-snapshotter/util/ioutils"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

type zstdCompression struct {
	*zstdchunked.Decompressor
	*zstdchunked.Compressor
}

// LayerConvertWithLayerOptsFunc converts legacy tar.gz layers into zstd:chunked layers.
//
// This changes Docker MediaType to OCI MediaType so this should be used in
// conjunction with WithDockerToOCI().
// See LayerConvertFunc for more details. The difference between this function and
// LayerConvertFunc is that this allows to specify additional eStargz options per layer.
func LayerConvertWithLayerOptsFunc(opts map[digest.Digest][]estargz.Option) converter.ConvertFunc {
	return LayerConvertWithLayerOptsFuncWithCompressionLevel(zstd.SpeedDefault, opts)
}

// LayerConvertFunc converts legacy tar.gz layers into zstd:chunked layers.
//
// This changes Docker MediaType to OCI MediaType so this should be used in
// conjunction with WithDockerToOCI().
//
// Otherwise "io.containers.zstd-chunked.manifest-checksum" annotation will be lost,
// because the Docker media type does not support layer annotations.
//
// SpeedDefault (level 3) is used for the compression level.
// See also: https://pkg.go.dev/github.com/klauspost/compress/zstd#EncoderLevel
func LayerConvertFunc(opts ...estargz.Option) converter.ConvertFunc {
	return LayerConvertFuncWithCompressionLevel(zstd.SpeedDefault, opts...)
}

// LayerConvertWithLayerOptsFuncWithCompressionLevel converts legacy tar.gz layers into zstd:chunked layers.
// This function allows to specify the compression level of zstd.
//
// This changes Docker MediaType to OCI MediaType so this should be used in
// conjunction with WithDockerToOCI().
// See LayerConvertFunc for more details. The difference between this function and
// LayerConvertFunc is that this allows to specify additional eStargz options per layer and
// allows to specify the compression level.
func LayerConvertWithLayerOptsFuncWithCompressionLevel(compressionLevel zstd.EncoderLevel, opts map[digest.Digest][]estargz.Option) converter.ConvertFunc {
	if opts == nil {
		return LayerConvertFuncWithCompressionLevel(compressionLevel)
	}
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		// TODO: enable to speciy option per layer "index" because it's possible that there are
		//       two layers having same digest in an image (but this should be rare case)
		return LayerConvertFuncWithCompressionLevel(compressionLevel, opts[desc.Digest]...)(ctx, cs, desc)
	}
}

// LayerConvertFuncWithCompressionLevel converts legacy tar.gz layers into zstd:chunked layers with
// the specified compression level.
//
// This changes Docker MediaType to OCI MediaType so this should be used in
// conjunction with WithDockerToOCI().
// See LayerConvertFunc for more details. The difference between this function and
// LayerConvertFunc is that this allows configuring the compression level.
func LayerConvertFuncWithCompressionLevel(compressionLevel zstd.EncoderLevel, opts ...estargz.Option) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		if !images.IsLayerType(desc.MediaType) {
			// No conversion. No need to return an error here.
			return nil, nil
		}
		uncompressedDesc := &desc
		// We need to uncompress the archive first
		if !uncompress.IsUncompressedType(desc.MediaType) {
			var err error
			uncompressedDesc, err = uncompress.LayerConvertFunc(ctx, cs, desc)
			if err != nil {
				return nil, err
			}
			if uncompressedDesc == nil {
				return nil, fmt.Errorf("unexpectedly got the same blob after compression (%s, %q)", desc.Digest, desc.MediaType)
			}
			defer func() {
				if err := cs.Delete(ctx, uncompressedDesc.Digest); err != nil {
					logrus.WithError(err).WithField("uncompressedDesc", uncompressedDesc).Warn("failed to remove tmp uncompressed layer")
				}
			}()
			logrus.Debugf("zstdchunked: uncompressed %s into %s", desc.Digest, uncompressedDesc.Digest)
		}

		info, err := cs.Info(ctx, desc.Digest)
		if err != nil {
			return nil, err
		}
		labelz := info.Labels
		if labelz == nil {
			labelz = make(map[string]string)
		}

		uncompressedReaderAt, err := cs.ReaderAt(ctx, *uncompressedDesc)
		if err != nil {
			return nil, err
		}
		defer uncompressedReaderAt.Close()
		uncompressedSR := io.NewSectionReader(uncompressedReaderAt, 0, uncompressedDesc.Size)
		metadata := make(map[string]string)
		opts = append(opts, estargz.WithCompression(&zstdCompression{
			new(zstdchunked.Decompressor),
			&zstdchunked.Compressor{
				CompressionLevel: compressionLevel,
				Metadata:         metadata,
			},
		}))
		blob, err := estargz.Build(uncompressedSR, append(opts, estargz.WithContext(ctx))...)
		if err != nil {
			return nil, err
		}
		defer blob.Close()
		ref := fmt.Sprintf("convert-zstdchunked-from-%s", desc.Digest)
		w, err := cs.Writer(ctx, content.WithRef(ref))
		if err != nil {
			return nil, err
		}
		defer w.Close()

		// Reset the writing position
		// Old writer possibly remains without aborted
		// (e.g. conversion interrupted by a signal)
		if err := w.Truncate(0); err != nil {
			return nil, err
		}

		// Copy and count the contents
		pr, pw := io.Pipe()
		c := new(ioutils.CountWriter)
		doneCount := make(chan struct{})
		go func() {
			defer close(doneCount)
			defer pr.Close()
			decompressR, err := compression.DecompressStream(pr)
			if err != nil {
				pr.CloseWithError(err)
				return
			}
			defer decompressR.Close()
			if _, err := io.Copy(c, decompressR); err != nil {
				pr.CloseWithError(err)
				return
			}
		}()
		n, err := io.Copy(w, io.TeeReader(blob, pw))
		if err != nil {
			return nil, err
		}
		if err := blob.Close(); err != nil {
			return nil, err
		}
		// update diffID label
		labelz[labels.LabelUncompressed] = blob.DiffID().String()
		if err = w.Commit(ctx, n, "", content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		newDesc := desc
		newDesc.MediaType, err = convertMediaTypeToZstd(newDesc.MediaType)
		if err != nil {
			return nil, err
		}
		newDesc.Digest = w.Digest()
		newDesc.Size = n
		if newDesc.Annotations == nil {
			newDesc.Annotations = make(map[string]string, 1)
		}
		tocDgst := blob.TOCDigest().String()
		newDesc.Annotations[estargz.TOCJSONDigestAnnotation] = tocDgst
		newDesc.Annotations[estargz.StoreUncompressedSizeAnnotation] = fmt.Sprintf("%d", c.Size())
		if p, ok := metadata[zstdchunked.ManifestChecksumAnnotation]; ok {
			newDesc.Annotations[zstdchunked.ManifestChecksumAnnotation] = p
		}
		if p, ok := metadata[zstdchunked.ManifestPositionAnnotation]; ok {
			newDesc.Annotations[zstdchunked.ManifestPositionAnnotation] = p
		}
		return &newDesc, nil
	}
}

// NOTE: this converts docker mediatype to OCI mediatype
func convertMediaTypeToZstd(mt string) (string, error) {
	ociMediaType := converter.ConvertDockerMediaTypeToOCI(mt)
	switch ociMediaType {
	case ocispec.MediaTypeImageLayer, ocispec.MediaTypeImageLayerGzip, ocispec.MediaTypeImageLayerZstd:
		return ocispec.MediaTypeImageLayerZstd, nil
	case ocispec.MediaTypeImageLayerNonDistributable, ocispec.MediaTypeImageLayerNonDistributableGzip, ocispec.MediaTypeImageLayerNonDistributableZstd:
		return ocispec.MediaTypeImageLayerNonDistributableZstd, nil
	default:
		return "", fmt.Errorf("unknown mediatype %q", mt)
	}
}
