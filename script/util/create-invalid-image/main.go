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

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	digest "github.com/opencontainers/go-digest"
	imagespecversioned "github.com/opencontainers/image-spec/specs-go"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

func main() {
	isEstargz := flag.Bool("estargz", false, "")

	flag.Parse()

	ctx := context.Background()
	ctx = namespaces.WithNamespace(ctx, "default")
	client, err := containerd.New(defaults.DefaultAddress)
	if err != nil {
		panic(err)
	}

	args := flag.Args()
	fmt.Println("args", args)
	targetImageRef := args[0]
	newRef := args[1]

	targetImage, err := client.GetImage(ctx, targetImageRef)
	if err != nil {
		panic(err)
	}
	originalManifet, err := images.Manifest(ctx, client.ContentStore(), targetImage.Target(), platforms.Default())
	if err != nil {
		panic(err)
	}

	layer, err := content.ReadBlob(ctx, client.ContentStore(), originalManifet.Layers[0])
	if err != nil {
		panic(err)
	}

	var modifiedLayer bytes.Buffer
	twl := tar.NewWriter(&modifiedLayer)
	gz, _ := gzip.NewReader(bytes.NewReader(layer))
	orgR := tar.NewReader(gz)
	for {
		h, err := orgR.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
		if err := twl.WriteHeader(h); err != nil {
			panic(err)
		}
		if _, err := io.Copy(twl, orgR); err != nil {
			panic(err)
		}
	}
	textContents := "MODIFIED"
	if err := twl.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "/modified.txt",
		Size:     int64(len(textContents)),
	}); err != nil {
		panic(err)
	}
	if _, err := twl.Write([]byte(textContents)); err != nil {
		panic(err)
	}
	twl.Close()

	modifiedLayerData := modifiedLayer.Bytes()
	modifiedLayerDesc := imagespec.Descriptor{
		MediaType: imagespec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(modifiedLayerData),
		Size:      int64(len(modifiedLayerData)),
	}
	if *isEstargz {
		esgzR, err := estargz.Build(io.NewSectionReader(bytes.NewReader(modifiedLayerData), 0, int64(len(modifiedLayerData))))
		if err != nil {
			panic(err)
		}
		defer esgzR.Close()
		modifiedLayerData, err = io.ReadAll(esgzR)
		if err != nil {
			panic(err)
		}
		modifiedLayerDesc = imagespec.Descriptor{
			MediaType: imagespec.MediaTypeImageLayerGzip,
			Digest:    digest.FromBytes(modifiedLayerData),
			Size:      int64(len(modifiedLayerData)),
			Annotations: map[string]string{
				estargz.TOCJSONDigestAnnotation: esgzR.TOCDigest().String(),
			},
		}
	}

	if err := content.WriteBlob(ctx, client.ContentStore(), "layer-modified", bytes.NewReader(modifiedLayerData), modifiedLayerDesc); err != nil {
		panic(err)
	}

	originalConfig, err := images.Config(ctx, client.ContentStore(), targetImage.Target(), platforms.Default())
	if err != nil {
		panic(err)
	}
	manifest := &imagespec.Manifest{
		Versioned: imagespecversioned.Versioned{2},
		MediaType: imagespec.MediaTypeImageManifest,
		Config:    originalConfig,
		Layers:    []imagespec.Descriptor{modifiedLayerDesc},
	}
	manifestBlob, err := json.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	manifestDesc := imagespec.Descriptor{
		MediaType: imagespec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestBlob),
		Size:      int64(len(manifestBlob)),
	}

	labels := make(map[string]string)
	for i, d := range append(manifest.Layers, manifest.Config) {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i)] = d.Digest.String()
	}
	if err := content.WriteBlob(ctx, client.ContentStore(), "manifest", bytes.NewReader(manifestBlob), manifestDesc, content.WithLabels(labels)); err != nil {
		panic(err)
	}

	i, err := client.ImageService().Create(ctx, images.Image{
		Name:   newRef,
		Target: manifestDesc,
	})
	if err != nil {
		panic(err)
	}
	containerd.NewImage(client, i)
}
