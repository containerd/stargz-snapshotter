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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package optimizer

import (
	"compress/gzip"
	gocontext "context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/analyzer"
	"github.com/containerd/stargz-snapshotter/analyzer/sampler"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/layerconverter"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/recorder"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/util"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	ocidigest "github.com/opencontainers/go-digest"
	spec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type Opts struct {
	Reuse  bool
	Period time.Duration
}

func Optimize(ctx gocontext.Context, opts *Opts, srcImg regpkg.Image, tf *tempfiles.TempFiles, rec *recorder.Recorder, samplerOpts ...sampler.Option) ([]mutate.Addendum, error) {
	// Get image's basic information
	manifest, err := srcImg.Manifest()
	if err != nil {
		return nil, err
	}
	configData, err := srcImg.RawConfigFile()
	if err != nil {
		return nil, err
	}
	var config spec.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, errors.Wrap(err, "failed to parse image config file")
	}
	// The order is base layer first, top layer last.
	in, err := srcImg.Layers()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get image layers")
	}

	// Unpack layers
	var (
		eg                 errgroup.Group
		decompressedLayers = make([]*io.SectionReader, len(in))
		compressedLayers   = make([]*io.SectionReader, len(in))
		mu                 sync.Mutex
	)
	for i, layer := range in {
		i, layer := i, layer
		eg.Go(func() error {
			// TODO: These files should be deduplicated.
			compressedFile, err := tf.TempFile("", "compresseddata")
			if err != nil {
				return err
			}
			decompressedFile, err := tf.TempFile("", "decompresseddata")
			if err != nil {
				return err
			}
			r, err := layer.Compressed()
			if err != nil {
				return err
			}
			defer r.Close()
			zr, err := gzip.NewReader(io.TeeReader(r, compressedFile))
			if err != nil {
				return err
			}
			defer zr.Close()
			if _, err := io.Copy(decompressedFile, zr); err != nil {
				return err
			}
			decompressedLayer, err := util.FileSectionReader(decompressedFile)
			if err != nil {
				return err
			}
			compressedLayer, err := util.FileSectionReader(compressedFile)
			if err != nil {
				return err
			}

			mu.Lock()
			decompressedLayers[i] = decompressedLayer
			compressedLayers[i] = compressedLayer
			mu.Unlock()

			dgst, err := layer.Digest()
			if err != nil {
				return err
			}
			log.G(ctx).WithField("digest", dgst).Infof("unpacked")
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// Analyze prioritized files
	var layerReaderAts []io.ReaderAt
	for _, r := range decompressedLayers {
		layerReaderAts = append(layerReaderAts, r)
	}
	logs, err := analyzer.Analyze(layerReaderAts, config,
		analyzer.WithSamplerOpts(samplerOpts...),
		analyzer.WithPeriod(opts.Period),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to analyze")
	}

	// Convert layers
	var (
		adds   = make([]mutate.Addendum, len(in))
		addsMu sync.Mutex
	)
	for i, layer := range in {
		i, layer := i, layer
		var (
			prioritizedFiles = logs[i]
			compressedL      = compressedLayers[i]
			decompressedL    = decompressedLayers[i]
		)
		dgst, err := layer.Digest()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get digest of layer")
		}
		ctx := log.WithLogger(ctx, log.G(ctx).WithField("digest", dgst))
		eg.Go(func() error {
			var converters []layerconverter.LayerConverter
			if tocdgst, ok := getTOCDigest(manifest, dgst); ok && opts.Reuse {
				// If this layer is a valid eStargz, try to reuse this layer.
				// If no access occur to this layer during the specified workload,
				// this layer will be reused without conversion.
				f, err := layerconverter.FromEStargz(ctx, tocdgst, layer, compressedL)
				if err == nil {
					// TODO: remotely mount it instead of downloading the layer.
					converters = append(converters, f)
				}
			}
			convert := layerconverter.Compose(append(converters,
				layerconverter.FromTar(ctx, decompressedL, tf))...)
			addendum, err := convert(prioritizedFiles)
			if err != nil {
				return errors.Wrap(err, "failed to get converted layer")
			}
			addsMu.Lock()
			adds[i] = addendum
			addsMu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// Dump records if required
	if rec != nil {
		manifestDigest, err := srcImg.Digest()
		if err != nil {
			return nil, err
		}
		manifestDigestStr := manifestDigest.String()
		for i := range in {
			i := i
			for _, f := range logs[i] {
				e := &recorder.Entry{
					Path:           f,
					ManifestDigest: manifestDigestStr,
					LayerIndex:     &i,
				}
				if err := rec.Record(e); err != nil {
					return nil, err
				}
			}
		}
	}

	return adds, nil
}

func getTOCDigest(manifest *regpkg.Manifest, dgst regpkg.Hash) (ocidigest.Digest, bool) {
	if manifest == nil {
		return "", false
	}
	for _, desc := range manifest.Layers {
		if desc.Digest.Algorithm == dgst.Algorithm && desc.Digest.Hex == dgst.Hex {
			dgstStr, ok := desc.Annotations[estargz.TOCJSONDigestAnnotation]
			if ok {
				if tocdgst, err := ocidigest.Parse(dgstStr); err == nil {
					return tocdgst, true
				}
			}
		}
	}
	return "", false
}
