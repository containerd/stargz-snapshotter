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

package imageio

import (
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ImageIO is an interface for helpers of reading/writing images to/from somewhere.
type ImageIO interface {
	ReadIndex() (regpkg.ImageIndex, error)
	WriteIndex(index regpkg.ImageIndex) error
	ReadImage() (regpkg.Image, error)
	WriteImage(image regpkg.Image) error
}

// RemoteImage is a helper for reading/writing images stored in the remote registry.
type RemoteImage struct {
	RemoteRef name.Reference
}

func (ri RemoteImage) ReadIndex() (regpkg.ImageIndex, error) {
	return remote.Index(ri.RemoteRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

func (ri RemoteImage) WriteIndex(index regpkg.ImageIndex) error {
	return remote.WriteIndex(ri.RemoteRef, index, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

func (ri RemoteImage) ReadImage() (regpkg.Image, error) {
	desc, err := remote.Get(ri.RemoteRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, err
	}
	return desc.Image()
}

func (ri RemoteImage) WriteImage(image regpkg.Image) error {
	return remote.Write(ri.RemoteRef, image, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

// LocalImage is a helper for reading/writing images stored in the OCI Image Layout directory.
type LocalImage struct {
	LocalPath string
}

func (li LocalImage) ReadIndex() (regpkg.ImageIndex, error) {
	lp, err := layout.FromPath(li.LocalPath)
	if err != nil {
		return nil, err
	}
	return lp.ImageIndex()
}

func (li LocalImage) WriteIndex(index regpkg.ImageIndex) error {
	_, err := layout.Write(li.LocalPath, index)
	return err
}

func (li LocalImage) ReadImage() (regpkg.Image, error) {
	// OCI Image Layout doesn't have representation of thin image
	return nil, fmt.Errorf("thin image cannot be read from local")
}

func (li LocalImage) WriteImage(image regpkg.Image) error {
	// OCI layout requires index so create it.
	// TODO: Should we add platform information here?
	desc, err := partial.Descriptor(image)
	if err != nil {
		return err
	}
	_, err = layout.Write(li.LocalPath, mutate.AppendManifests(
		empty.Index,
		mutate.IndexAddendum{
			Add:        image,
			Descriptor: *desc,
		},
	))
	return err
}
