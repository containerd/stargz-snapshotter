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

package layerconverter

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/layer"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/logger"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/hashicorp/go-multierror"
	ocidigest "github.com/opencontainers/go-digest"
)

type LayerConverter = func() (mutate.Addendum, error)

func FromTar(ctx context.Context, sr *io.SectionReader, mon logger.Monitor, tf *tempfiles.TempFiles) LayerConverter {
	return func() (mutate.Addendum, error) {
		log.G(ctx).Debugf("converting...")
		defer log.G(ctx).Infof("converted")

		rc, jtocDigest, err := estargz.Build(sr, mon.DumpLog())
		if err != nil {
			return mutate.Addendum{}, err
		}
		defer rc.Close()
		log.G(ctx).WithField("TOC JSON digest", jtocDigest).Debugf("calculated digest")
		l, err := layer.NewStaticCompressedLayer(rc, tf)
		if err != nil {
			return mutate.Addendum{}, err
		}
		return mutate.Addendum{
			Layer: l,
			Annotations: map[string]string{
				estargz.TOCJSONDigestAnnotation: jtocDigest.String(),
			},
		}, nil
	}
}

func FromEStargz(ctx context.Context, tocdgst ocidigest.Digest, l regpkg.Layer, sr *io.SectionReader, mon logger.Monitor) (LayerConverter, error) {
	// If the layer is valid eStargz, use this layer without conversion
	r, err := estargz.Open(sr)
	if err != nil {
		return nil, err
	}
	if _, err := r.VerifyTOC(tocdgst); err != nil {
		return nil, err
	}
	dgst, err := l.Digest()
	if err != nil {
		return nil, err
	}
	diff, err := l.DiffID()
	if err != nil {
		return nil, err
	}
	return func() (mutate.Addendum, error) {
		if len(mon.DumpLog()) != 0 {
			// There have been some accesses to this layer. we don't reuse this.
			return mutate.Addendum{}, fmt.Errorf("unable to reuse accessed layer")
		}
		log.G(ctx).Infof("no access occur; copying without conversion")
		return mutate.Addendum{
			Layer: layer.StaticCompressedLayer{
				R:       sr,
				Diff:    diff,
				Hash:    dgst,
				SizeVal: sr.Size(),
			},
			Annotations: map[string]string{
				estargz.TOCJSONDigestAnnotation: tocdgst.String(),
			},
		}, nil
	}, nil
}

func Compose(cs ...LayerConverter) LayerConverter {
	return func() (add mutate.Addendum, allErr error) {
		for _, f := range cs {
			a, err := f()
			if err == nil {
				return a, nil
			}
			allErr = multierror.Append(allErr, err)
		}
		return
	}
}
