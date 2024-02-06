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
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/testutil"
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

	lcf := LayerConvertFunc(estargz.WithPrioritizedFiles([]string{"hello"}))
	docker2oci := true
	platformMC := platforms.DefaultStrict()
	cf := converter.DefaultIndexConvertFunc(lcf, docker2oci, platformMC)

	newDesc, err := cf(ctx, cs, *desc)
	if err != nil {
		t.Fatal(err)
	}

	var tocDigests []string
	handler := func(hCtx context.Context, hDesc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
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
}
