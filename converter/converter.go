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

package converter

import (
	gocontext "context"
	"fmt"
	"io"
	"sync"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/analyzer/sampler"
	"github.com/containerd/stargz-snapshotter/converter/optimizer"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/layer"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/recorder"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/util"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	ocidigest "github.com/opencontainers/go-digest"
	spec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

func ConvertIndex(ctx gocontext.Context, noOptimize bool, opts *optimizer.Opts, srcIndex regpkg.ImageIndex, platform *spec.Platform, tf *tempfiles.TempFiles, rec *recorder.Recorder, runopts ...sampler.Option) (regpkg.ImageIndex, error) {
	var addendums []mutate.IndexAddendum
	manifest, err := srcIndex.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, m := range manifest.Manifests {
		p := platforms.DefaultSpec()
		if m.Platform != nil {
			p = *(specPlatform(m.Platform))
		}
		if platform != nil {
			if !platforms.NewMatcher(*platform).Match(p) {
				continue
			}
		}
		srcImg, err := srcIndex.Image(m.Digest)
		if err != nil {
			return nil, err
		}
		cctx := log.WithLogger(ctx, log.G(ctx).WithField("platform", platforms.Format(p)))
		dstImg, err := ConvertImage(cctx, noOptimize, opts, srcImg, &p, tf, rec, runopts...)
		if err != nil {
			return nil, err
		}
		desc, err := partial.Descriptor(dstImg)
		if err != nil {
			return nil, err
		}
		desc.Platform = m.Platform // inherit the platform information
		addendums = append(addendums, mutate.IndexAddendum{
			Add:        dstImg,
			Descriptor: *desc,
		})
	}
	if len(addendums) == 0 {
		return nil, fmt.Errorf("no target image is specified")
	}

	// Push the converted image
	return mutate.AppendManifests(empty.Index, addendums...), nil
}

func ConvertImage(ctx gocontext.Context, noOptimize bool, opts *optimizer.Opts, srcImg regpkg.Image, platform *spec.Platform, tf *tempfiles.TempFiles, rec *recorder.Recorder, runopts ...sampler.Option) (dstImg regpkg.Image, _ error) {
	// The order of the list is base layer first, top layer last.
	layers, err := srcImg.Layers()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get image layers")
	}
	addendums := make([]mutate.Addendum, len(layers))
	if noOptimize || !platforms.NewMatcher(platforms.DefaultSpec()).Match(*platform) {
		// Do not run the optimization container if the option requires it or
		// the source image doesn't match to the platform where this command runs on.
		log.G(ctx).Warn("Platform mismatch or optimization disabled; converting without optimization")
		// TODO: enable to reuse layers
		var eg errgroup.Group
		var addendumsMu sync.Mutex
		for i, l := range layers {
			i, l := i, l
			eg.Go(func() error {
				newL, jtocDigest, err := buildEStargzLayer(l, tf)
				if err != nil {
					return err
				}
				addendumsMu.Lock()
				addendums[i] = mutate.Addendum{
					Layer: newL,
					Annotations: map[string]string{
						estargz.TOCJSONDigestAnnotation: jtocDigest.String(),
					},
				}
				addendumsMu.Unlock()
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, errors.Wrapf(err, "failed to convert layer to stargz")
		}
	} else {
		addendums, err = optimizer.Optimize(ctx, opts, srcImg, tf, rec, runopts...)
		if err != nil {
			return nil, err
		}
	}
	srcCfg, err := srcImg.ConfigFile()
	if err != nil {
		return nil, err
	}
	srcCfg.RootFS.DiffIDs = []regpkg.Hash{}
	srcCfg.History = []regpkg.History{}
	img, err := mutate.ConfigFile(empty.Image, srcCfg)
	if err != nil {
		return nil, err
	}

	return mutate.Append(img, addendums...)
}

func buildEStargzLayer(uncompressed regpkg.Layer, tf *tempfiles.TempFiles) (regpkg.Layer, ocidigest.Digest, error) {
	tftmp := tempfiles.NewTempFiles() // Shorter lifetime than tempfiles passed by argument
	defer tftmp.CleanupAll()
	r, err := uncompressed.Uncompressed()
	if err != nil {
		return nil, "", err
	}
	file, err := tftmp.TempFile("", "tmpdata")
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(file, r); err != nil {
		return nil, "", err
	}
	sr, err := util.FileSectionReader(file)
	if err != nil {
		return nil, "", err
	}
	rc, err := estargz.Build(sr) // no optimization
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	l, err := layer.NewStaticCompressedLayer(rc, tf)
	if err != nil {
		return nil, "", err
	}
	return l, rc.TOCDigest(), err
}

// specPlatform converts ggcr's platform struct to OCI's struct
func specPlatform(p *regpkg.Platform) *spec.Platform {
	return &spec.Platform{
		Architecture: p.Architecture,
		OS:           p.OS,
		OSVersion:    p.OSVersion,
		OSFeatures:   p.OSFeatures,
		Variant:      p.Variant,
	}
}
