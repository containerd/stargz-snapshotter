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

package externaltoc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/images/converter/uncompress"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/stargz-snapshotter/estargz"
	esgzexternaltoc "github.com/containerd/stargz-snapshotter/estargz/externaltoc"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	"github.com/containerd/stargz-snapshotter/util/ioutils"
	"github.com/opencontainers/go-digest"
	ocispecspec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerConvertFunc converts legacy tar.gz layers into eStargz tar.gz layers.
//
// finalize() callback function returned by this function will return the image that contains
// external TOC of each layer. Note that the returned image by isn't stored to the containerd image
// store so far so the caller needs to do it.
//
// Media type is unchanged.
//
// Should be used in conjunction with WithDockerToOCI().
//
// Otherwise "containerd.io/snapshot/stargz/toc.digest" annotation will be lost,
// because the Docker media type does not support layer annotations.
//
// WithCompression() in esgzOpts will be ignored but used the one for external TOC instead.
func LayerConvertFunc(esgzOpts []estargz.Option, compressionLevel int) (convertFunc converter.ConvertFunc, finalize func(ctx context.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)) {
	return layerConvert(func(c estargz.Compression) converter.ConvertFunc {
		return estargzconvert.LayerConvertFunc(append(esgzOpts, estargz.WithCompression(c))...)
	}, compressionLevel)
}

// LayerConvertWithLayerAndCommonOptsFunc converts legacy tar.gz layers into eStargz.
// Media type is unchanged. Should be used in conjunction with WithDockerToOCI(). See
// LayerConvertFunc for more details. The difference between this function and
// LayerConvertFunc is that this allows to specify additional eStargz options per layer.
func LayerConvertWithLayerAndCommonOptsFunc(opts map[digest.Digest][]estargz.Option, commonOpts []estargz.Option, compressionLevel int) (convertFunc converter.ConvertFunc, finalize func(ctx context.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)) {
	return layerConvert(func(c estargz.Compression) converter.ConvertFunc {
		return estargzconvert.LayerConvertWithLayerAndCommonOptsFunc(opts, append(commonOpts,
			estargz.WithCompression(c),
		)...)
	}, compressionLevel)
}

// LayerConvertLossLessConfig is configuration for LayerConvertLossLessFunc.
type LayerConvertLossLessConfig struct {
	CompressionLevel int
	ChunkSize        int
	MinChunkSize     int
}

// LayerConvertLossLessFunc converts legacy tar.gz layers into eStargz tar.gz layers without changing
// the diffIDs (i.e. uncompressed digest).
//
// finalize() callback function returned by this function will return the image that contains
// external TOC of each layer. Note that the returned image by isn't stored to the containerd image
// store so far so the caller needs to do it.
//
// Media type is unchanged.
//
// Should be used in conjunction with WithDockerToOCI().
//
// Otherwise "containerd.io/snapshot/stargz/toc.digest" annotation will be lost,
// because the Docker media type does not support layer annotations.
//
// WithCompression() in esgzOpts will be ignored but used the one for external TOC instead.
func LayerConvertLossLessFunc(cfg LayerConvertLossLessConfig) (convertFunc converter.ConvertFunc, finalize func(ctx context.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)) {
	return layerConvert(func(c estargz.Compression) converter.ConvertFunc {
		return layerLossLessConvertFunc(c, cfg.ChunkSize, cfg.MinChunkSize)
	}, cfg.CompressionLevel)
}

func layerConvert(layerConvertFunc func(estargz.Compression) converter.ConvertFunc, compressionLevel int) (convertFunc converter.ConvertFunc, finalize func(ctx context.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)) {
	type tocInfo struct {
		digest digest.Digest
		size   int64
	}
	esgzDigest2TOC := make(map[digest.Digest]tocInfo)
	// TODO: currently, all layers of all platforms are combined to one TOC manifest. Maybe we can consider
	//       having a separated TOC manifest per platform.
	converterFunc := func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		cm := esgzexternaltoc.NewGzipCompressionWithLevel(nil, compressionLevel)
		c := cm.(*esgzexternaltoc.GzipCompression)
		cf := layerConvertFunc(c)
		desc2, err := cf(ctx, cs, desc)
		if err != nil {
			return desc2, err
		}
		var layerDgst digest.Digest
		if desc2 != nil {
			layerDgst = desc2.Digest
		} else {
			layerDgst = desc.Digest // no conversion happened
		}
		dgst, size, err := writeTOCTo(ctx, c, cs)
		if err != nil {
			return nil, err
		}
		esgzDigest2TOC[layerDgst] = tocInfo{dgst, size}
		return desc2, nil
	}
	finalizeFunc := func(ctx context.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error) {
		var layers []ocispec.Descriptor
		for esgzDigest, toc := range esgzDigest2TOC {
			layers = append(layers, ocispec.Descriptor{
				MediaType: ocispec.MediaTypeImageLayerGzip,
				Digest:    toc.digest,
				Size:      toc.size,
				Annotations: map[string]string{
					"containerd.io/snapshot/stargz/layer.digest": esgzDigest.String(),
				},
			})
		}
		sort.Slice(layers, func(i, j int) bool {
			return layers[i].Digest.String() < layers[j].Digest.String()
		})
		mfst, err := createManifest(ctx, cs, ocispec.ImageConfig{}, layers)
		if err != nil {
			return nil, err
		}
		tocImgRef, err := getTOCReference(ref)
		if err != nil {
			return nil, err
		}
		return &images.Image{
			Name:   tocImgRef,
			Target: *mfst,
		}, nil
	}
	return converterFunc, finalizeFunc
}

func getTOCReference(ref string) (string, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return "", err
	}
	refspec.Object = refspec.Object + "-esgztoc" // TODO: support custom location
	return refspec.String(), nil
}

func layerLossLessConvertFunc(compressor estargz.Compressor, chunkSize int, minChunkSize int) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		if !images.IsLayerType(desc.MediaType) {
			// No conversion. No need to return an error here.
			return nil, nil
		}
		info, err := cs.Info(ctx, desc.Digest)
		if err != nil {
			return nil, err
		}
		labelz := info.Labels
		if labelz == nil {
			labelz = make(map[string]string)
		}

		ra, err := cs.ReaderAt(ctx, desc)
		if err != nil {
			return nil, err
		}
		defer ra.Close()
		sr := io.NewSectionReader(ra, 0, desc.Size)
		ref := fmt.Sprintf("convert-estargz-from-%s", desc.Digest)
		w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
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
		esgzUW, esgzUncompressedInfoCh := calcUncompression()
		orgUW, orgUncompressedInfoCh := calcUncompression()
		countW := new(ioutils.CountWriter)
		mw := io.MultiWriter(io.MultiWriter(w, countW), esgzUW)
		var ew *estargz.Writer
		if compressor != nil {
			ew = estargz.NewWriterWithCompressor(mw, compressor)
		} else {
			ew = estargz.NewWriter(mw)
		}
		if chunkSize > 0 {
			ew.ChunkSize = chunkSize
		}
		ew.MinChunkSize = minChunkSize
		if err := ew.AppendTarLossLess(io.TeeReader(sr, orgUW)); err != nil {
			return nil, fmt.Errorf("cannot perform compression in lossless way: %w", err)
		}
		tocDgst, err := ew.Close()
		if err != nil {
			return nil, err
		}
		n := countW.Size()
		if err := esgzUW.Close(); err != nil {
			return nil, err
		}
		if err := orgUW.Close(); err != nil {
			return nil, err
		}
		esgzUncompressedInfo := <-esgzUncompressedInfoCh
		orgUncompressedInfo := <-orgUncompressedInfoCh

		// check the lossless conversion
		if esgzUncompressedInfo.diffID.String() != orgUncompressedInfo.diffID.String() {
			return nil, fmt.Errorf("unexpected diffID %q; want %q",
				esgzUncompressedInfo.diffID.String(), orgUncompressedInfo.diffID.String())
		}
		if esgzUncompressedInfo.size != orgUncompressedInfo.size {
			return nil, fmt.Errorf("unexpected uncompressed size %q; want %q",
				esgzUncompressedInfo.size, orgUncompressedInfo.size)
		}

		// write diffID label
		labelz[labels.LabelUncompressed] = esgzUncompressedInfo.diffID.String()
		if err = w.Commit(ctx, n, "", content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		newDesc := desc
		if uncompress.IsUncompressedType(newDesc.MediaType) {
			if images.IsDockerType(newDesc.MediaType) {
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
		newDesc.Annotations[estargz.TOCJSONDigestAnnotation] = tocDgst.String()
		newDesc.Annotations[estargz.StoreUncompressedSizeAnnotation] = fmt.Sprintf("%d", esgzUncompressedInfo.size)
		return &newDesc, nil
	}
}

type uncompressedInfo struct {
	diffID digest.Digest
	size   int64
}

func calcUncompression() (*io.PipeWriter, chan uncompressedInfo) {
	pr, pw := io.Pipe()
	infoCh := make(chan uncompressedInfo)
	go func() {
		defer pr.Close()

		c := new(ioutils.CountWriter)
		diffID := digest.Canonical.Digester()
		decompressR, err := compression.DecompressStream(pr)
		if err != nil {
			pr.CloseWithError(err)
			close(infoCh)
			return
		}
		defer decompressR.Close()
		if _, err := io.Copy(io.MultiWriter(c, diffID.Hash()), decompressR); err != nil {
			pr.CloseWithError(err)
			close(infoCh)
			return
		}
		infoCh <- uncompressedInfo{
			diffID: diffID.Digest(),
			size:   c.Size(),
		}
	}()
	return pw, infoCh
}

func writeTOCTo(ctx context.Context, gc *esgzexternaltoc.GzipCompression, cs content.Store) (digest.Digest, int64, error) {
	ref := "external-toc" + time.Now().String()
	w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
	if err != nil {
		return "", 0, err
	}
	defer w.Close()
	if err := w.Truncate(0); err != nil {
		return "", 0, err
	}
	c := new(ioutils.CountWriter)
	dgstr := digest.Canonical.Digester()
	n, err := gc.WriteTOCTo(io.MultiWriter(io.MultiWriter(w, dgstr.Hash()), c))
	if err != nil {
		return "", 0, err
	}
	if err := w.Commit(ctx, int64(n), ""); err != nil && !errdefs.IsAlreadyExists(err) {
		return "", 0, err
	}
	if err := w.Close(); err != nil {
		return "", 0, err
	}

	return dgstr.Digest(), c.Size(), nil
}

func createManifest(ctx context.Context, cs content.Store, config ocispec.ImageConfig, layers []ocispec.Descriptor) (*ocispec.Descriptor, error) {
	// Create config
	configDgst, configSize, err := writeJSON(ctx, cs, &config, nil)
	if err != nil {
		return nil, err
	}

	// Create manifest
	mfst := ocispec.Manifest{
		Versioned: ocispecspec.Versioned{
			SchemaVersion: 2,
		},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDgst,
			Size:      configSize,
		},
		Layers: layers,
	}
	mfstLabels := make(map[string]string)
	for i, ld := range mfst.Layers {
		mfstLabels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = ld.Digest.String()
	}
	mfstLabels["containerd.io/gc.ref.content.c.0"] = configDgst.String()
	mfstDgst, mfstSize, err := writeJSON(ctx, cs, &mfst, mfstLabels)
	if err != nil {
		return nil, err
	}

	return &ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    mfstDgst,
		Size:      mfstSize,
	}, nil
}
func writeJSON(ctx context.Context, cs content.Store, data interface{}, labels map[string]string) (digest.Digest, int64, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return "", 0, err
	}
	size := len(raw)
	ref := "write-json-ref" + digest.FromBytes(raw).String()
	w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
	if err != nil {
		return "", 0, err
	}
	defer w.Close()
	if err := w.Truncate(0); err != nil {
		return "", 0, err
	}
	if _, err := w.Write(raw); err != nil {
		return "", 0, err
	}
	if err = w.Commit(ctx, int64(size), "", content.WithLabels(labels)); err != nil && !errdefs.IsAlreadyExists(err) {
		return "", 0, err
	}
	dgst := w.Digest()
	if err := w.Close(); err != nil {
		return "", 0, err
	}
	return dgst, int64(size), nil
}
