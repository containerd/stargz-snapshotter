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
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestLayerConvertFunc tests eStargz conversion.
// TestLayerConvertFunc is a pure unit test that does not need the daemon to be running.
func TestLayerConvertFunc(t *testing.T) {
	ctx := context.Background()
	desc, cs, err := testutil.EnsureHello(ctx)
	if err != nil {
		t.Fatal(err)
	}

	lcf, finalize := LayerConvertFunc(nil, gzip.BestSpeed)
	docker2oci := true
	platformMC := platforms.DefaultStrict()
	cf := converter.DefaultIndexConvertFunc(lcf, docker2oci, platformMC)

	newDesc, err := cf(ctx, cs, *desc)
	if err != nil {
		t.Fatal(err)
	}

	targetRef := "docker.io/library/hello:target"
	externalTOCImg, err := finalize(ctx, cs, targetRef, newDesc)
	if err != nil {
		t.Fatal(err)
	}
	if externalTOCImg.Name != targetRef+"-esgztoc" {
		t.Fatalf("external TOC ref; got = %q; wanted = %q", externalTOCImg.Name, targetRef+"-esgztoc")
	}

	layerDigests := make(map[digest.Digest]struct{})
	var tocDigests []string
	handler := func(hCtx context.Context, hDesc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if images.IsLayerType(hDesc.MediaType) {
			layerDigests[hDesc.Digest] = struct{}{}
		}
		if hDesc.Annotations != nil {
			if x, ok := hDesc.Annotations[estargz.TOCJSONDigestAnnotation]; ok && len(x) > 0 {
				tocDigests = append(tocDigests, x)
			}
		}
		return nil, nil
	}
	handlers := images.Handlers(
		images.ChildrenHandler(cs),
		images.HandlerFunc(handler),
	)
	if err := images.Walk(ctx, handlers, *newDesc); err != nil {
		t.Fatal(err)
	}

	if len(tocDigests) == 0 {
		t.Fatal("no eStargz layer was created")
	}

	ra, err := cs.ReaderAt(ctx, externalTOCImg.Target)
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()
	sr := io.NewSectionReader(ra, 0, ra.Size())
	var manifest ocispec.Manifest
	if err := json.NewDecoder(sr).Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	for _, l := range manifest.Layers {
		targetLayer, ok := l.Annotations["containerd.io/snapshot/stargz/layer.digest"]
		if !ok {
			t.Fatal("external TOC must contain target layer in annotation")
		}
		d, err := digest.Parse(targetLayer)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("GOT: %v", d)
		delete(layerDigests, d)
	}

	if len(layerDigests) != 0 {
		t.Fatalf("some layers don't have external TOC: %+v", layerDigests)
	}
}

// TestLayerConvertLossLessFunc tests eStargz conversion with lossless mode.
// TestLayerConvertLossLessFunc is a pure unit test that does not need the daemon to be running.
func TestLayerConvertLossLessFunc(t *testing.T) {
	ctx := context.Background()
	desc, cs, err := testutil.EnsureHello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	orgRootFS := rootFS(ctx, t, cs, desc)

	lcf, finalize := LayerConvertLossLessFunc(LayerConvertLossLessConfig{
		CompressionLevel: gzip.BestSpeed,
	})
	docker2oci := true
	platformMC := platforms.DefaultStrict()
	cf := converter.DefaultIndexConvertFunc(lcf, docker2oci, platformMC)

	newDesc, err := cf(ctx, cs, *desc)
	if err != nil {
		t.Fatal(err)
	}

	targetRef := "docker.io/library/hello:target"
	externalTOCImg, err := finalize(ctx, cs, targetRef, newDesc)
	if err != nil {
		t.Fatal(err)
	}
	if externalTOCImg.Name != targetRef+"-esgztoc" {
		t.Fatalf("external TOC ref; got = %q; wanted = %q", externalTOCImg.Name, targetRef+"-esgztoc")
	}
	newRootFS := rootFS(ctx, t, cs, newDesc)
	if len(orgRootFS) != len(newRootFS) {
		t.Fatalf("size of rootfs; got = %d; want = %d", len(orgRootFS), len(newRootFS))
	}
	for i := range orgRootFS {
		if orgRootFS[i] != newRootFS[i] {
			t.Fatalf("rootfs[%d]; got = %d; want = %d", i, len(orgRootFS), len(newRootFS))
		}
	}

	layerDigests := make(map[digest.Digest]struct{})
	wantDiffIDs := make(map[digest.Digest]struct{})
	for _, d := range orgRootFS {
		wantDiffIDs[d] = struct{}{}
	}
	var tocDigests []string
	handler := func(hCtx context.Context, hDesc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if images.IsLayerType(hDesc.MediaType) {
			layerDigests[hDesc.Digest] = struct{}{}
			ra, err := cs.ReaderAt(ctx, hDesc)
			if err != nil {
				t.Fatal(err)
			}
			defer ra.Close()
			gr, err := gzip.NewReader(io.NewSectionReader(ra, 0, ra.Size()))
			if err != nil {
				t.Fatal(err)
			}
			diffID, err := digest.FromReader(gr)
			if err != nil {
				t.Fatal(err)
			}
			delete(wantDiffIDs, diffID)
		}
		if hDesc.Annotations != nil {
			if x, ok := hDesc.Annotations[estargz.TOCJSONDigestAnnotation]; ok && len(x) > 0 {
				tocDigests = append(tocDigests, x)
			}
		}
		return nil, nil
	}
	handlers := images.Handlers(
		images.ChildrenHandler(cs),
		images.HandlerFunc(handler),
	)
	if err := images.Walk(ctx, handlers, *newDesc); err != nil {
		t.Fatal(err)
	}

	if len(wantDiffIDs) != 0 {
		t.Fatalf("some unexpected rootfs: %+v", wantDiffIDs)
	}

	if len(tocDigests) == 0 {
		t.Fatal("no eStargz layer was created")
	}

	ra, err := cs.ReaderAt(ctx, externalTOCImg.Target)
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()
	sr := io.NewSectionReader(ra, 0, ra.Size())
	var manifest ocispec.Manifest
	if err := json.NewDecoder(sr).Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	for _, l := range manifest.Layers {
		targetLayer, ok := l.Annotations["containerd.io/snapshot/stargz/layer.digest"]
		if !ok {
			t.Fatal("external TOC must contain target layer in annotation")
		}
		d, err := digest.Parse(targetLayer)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("GOT: %v", d)
		delete(layerDigests, d)
	}

	if len(layerDigests) != 0 {
		t.Fatalf("some layers don't have external TOC: %+v", layerDigests)
	}
}

func rootFS(ctx context.Context, t *testing.T, cs content.Store, desc *ocispec.Descriptor) []digest.Digest {
	conf, err := images.Config(ctx, cs, *desc, platforms.All)
	if err != nil {
		t.Fatal(err)
	}
	rfs, err := images.RootFS(ctx, cs, conf)
	if err != nil {
		t.Fatal(err)
	}
	return rfs
}
