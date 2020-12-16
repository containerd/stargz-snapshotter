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

package layer

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/ioutil"

	"github.com/containerd/stargz-snapshotter/converter/optimizer/util"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/pkg/errors"
)

func NewStaticCompressedLayer(compressed io.Reader, tf *tempfiles.TempFiles) (regpkg.Layer, error) {
	file, err := tf.TempFile("", "layerdata")
	if err != nil {
		return nil, err
	}
	var (
		diff = sha256.New()
		h    = sha256.New()
	)
	zr, err := gzip.NewReader(io.TeeReader(compressed, io.MultiWriter(file, h)))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if _, err := io.Copy(diff, zr); err != nil {
		return nil, err
	}
	sr, err := util.FileSectionReader(file)
	if err != nil {
		return nil, err
	}
	return StaticCompressedLayer{
		R: sr,
		Diff: regpkg.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(diff.Sum(nil)),
		},
		Hash: regpkg.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(h.Sum(nil)),
		},
		SizeVal: sr.Size(),
	}, nil
}

type StaticCompressedLayer struct {
	R       io.Reader
	Diff    regpkg.Hash
	Hash    regpkg.Hash
	SizeVal int64
}

func (l StaticCompressedLayer) Digest() (regpkg.Hash, error) {
	return l.Hash, nil
}

func (l StaticCompressedLayer) Size() (int64, error) {
	return l.SizeVal, nil
}

func (l StaticCompressedLayer) DiffID() (regpkg.Hash, error) {
	return l.Diff, nil
}

func (l StaticCompressedLayer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

func (l StaticCompressedLayer) Compressed() (io.ReadCloser, error) {
	// TODO: We should pass l.closerFunc to ggcr as Close() of io.ReadCloser
	//       but ggcr currently doesn't call Close() so we close it manually on EOF.
	//       See also: https://github.com/google/go-containerregistry/pull/768
	return ioutil.NopCloser(l.R), nil
}

func (l StaticCompressedLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("unsupported")
}
