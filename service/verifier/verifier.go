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

package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	esgzexternaltoc "github.com/containerd/stargz-snapshotter/nativeconverter/estargz/externaltoc"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func NewVerifier(
	rootDir string,
	client *containerd.Client,
	namespace string,
	resolver func(refpsec reference.Spec) remotes.Resolver,
	action func(ctx context.Context, chainID digest.Digest) error,
) *Verifier {
	return &Verifier{
		client:    client,
		namespace: namespace,
		resolver:  resolver,
		rootDir:   rootDir,
		action:    action,
	}
}

type Verifier struct {
	client    *containerd.Client
	namespace string
	resolver  func(refpsec reference.Spec) remotes.Resolver
	rootDir   string
	action    func(ctx context.Context, chainID digest.Digest) error
}

func (v *Verifier) Verify(ctx context.Context, diffIDs []digest.Digest, layers []ocispec.Descriptor) (bool, error) {
	ctx = namespaces.WithNamespace(ctx, v.namespace)
	if len(diffIDs) != len(layers) {
		return false, fmt.Errorf("invalid number of diffIDs len(diffIDs)=%d != len(layers)=%d", len(diffIDs), len(layers))
	}
	chainIDs := make([]digest.Digest, len(diffIDs))
	copy(chainIDs, diffIDs)
	chainIDs = identity.ChainIDs(chainIDs)

	sn := v.client.SnapshotService("stargz")
	for i := 0; i < len(chainIDs); i++ {
		info, err := sn.Stat(ctx, chainIDs[i].String())
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if err := func() error {
			tocDigest1, ok1 := info.Labels[estargz.TOCJSONDigestAnnotation]
			tocDigest2, ok2 := layers[i].Annotations[estargz.TOCJSONDigestAnnotation]
			if ok1 != ok2 {
				return fmt.Errorf("unmatched TOCDigest existence")
			}
			if !ok1 {
				return nil
			}
			if tocDigest1 != tocDigest2 {
				return fmt.Errorf("unmatched TOCDigest")
			}
			return nil
		}(); err == nil {
			continue
		}
		if isValid, err := v.isExistingSnapshotValid(ctx, diffIDs[i], info); err != nil {
			return false, err
		} else if isValid {
			// Refuse pulling this image.
			return false, nil
		}
		if err := v.action(ctx, chainIDs[i]); err != nil {
			return false, fmt.Errorf("failed to remove snapshots: %w", err)
		}
	}

	return true, nil
}

const (
	targetRefCRILabel         = "containerd.io/snapshot/cri.image-ref"
	targetLayerDigestCRILabel = "containerd.io/snapshot/cri.layer-digest"

	targetRefLabel    = "containerd.io/snapshot/remote/stargz.reference"
	targetDigestLabel = "containerd.io/snapshot/remote/stargz.digest"
)

func (v *Verifier) isExistingSnapshotValid(ctx context.Context, expectedDiffID digest.Digest, info snapshots.Info) (bool, error) {
	targetRef, refOk := info.Labels[targetRefCRILabel]
	targetLayerDigestStr, dgstOk := info.Labels[targetLayerDigestCRILabel]
	if !refOk || !dgstOk {
		targetRef, refOk = info.Labels[targetRefLabel]
		targetLayerDigestStr, dgstOk = info.Labels[targetDigestLabel]
		if !refOk || !dgstOk {
			return false, fmt.Errorf("target ref and digest label not found")
		}
	}
	targetLayerDigest, err := digest.Parse(targetLayerDigestStr)
	if err != nil {
		return false, err
	}

	refspec, err := reference.Parse(targetRef)
	if err != nil {
		return false, err
	}

	tocDigestStr, ok := info.Labels[estargz.TOCJSONDigestAnnotation]
	var tocDigest digest.Digest
	if ok {
		tocDigest, err = digest.Parse(tocDigestStr)
		if err != nil {
			return false, err
		}
	}

	return v.verifyLayer(ctx, refspec, targetLayerDigest, expectedDiffID, tocDigest)
}

func (v *Verifier) FetchImageManifest(ctx context.Context, refspec reference.Spec) (*ocispec.Manifest, *ocispec.Image, error) {
	resolver := v.resolver(refspec)
	_, img, err := resolver.Resolve(ctx, refspec.String())
	if err != nil {
		return nil, nil, err
	}
	fetcher, err := resolver.Fetcher(ctx, refspec.String())
	if err != nil {
		return nil, nil, err
	}
	manifest, err := containerdutil.FetchManifestPlatform(ctx, fetcher, img, platforms.DefaultSpec())
	if err != nil {
		return nil, nil, err
	}
	r, err := fetcher.Fetch(ctx, manifest.Config)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	var config ocispec.Image
	if err := json.NewDecoder(r).Decode(&config); err != nil {
		return nil, nil, err
	}

	return &manifest, &config, nil
}

func (v *Verifier) verifyLayer(ctx context.Context, refspec reference.Spec, targetLayerDigest digest.Digest, expectedDiffID digest.Digest, tocDigest digest.Digest) (bool, error) {
	resolver := v.resolver(refspec)
	if _, _, err := resolver.Resolve(ctx, refspec.String()); err != nil {
		return false, err
	}
	fetcher, err := resolver.Fetcher(ctx, refspec.String())
	if err != nil {
		return false, err
	}
	fetchReader, err := fetcher.Fetch(ctx, ocispec.Descriptor{Digest: targetLayerDigest, Size: -1})
	if err != nil {
		return false, err
	}

	if err := os.MkdirAll(v.rootDir, 0755); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(v.rootDir, "")
	if err != nil {
		return false, err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()

	r, err := compression.DecompressStream(io.TeeReader(fetchReader, f))
	if err != nil {
		return false, err
	}
	dgstr := digest.Canonical.Digester()
	if _, err := io.Copy(dgstr.Hash(), r); err != nil {
		return false, err
	}
	uncompressedDgst := dgstr.Digest()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}

	if expectedDiffID != uncompressedDgst {
		return false, nil
	}

	if tocDigest != "" {
		esgzR, err := estargz.Open(io.NewSectionReader(f, 0, st.Size()),
			estargz.WithDecompressors(
				new(zstdchunked.Decompressor),
				esgzexternaltoc.NewRemoteDecompressorWithResolver(ctx, resolver, refspec, ocispec.Descriptor{Digest: targetLayerDigest}),
			),
		)
		if err != nil {
			return false, err
		}
		if esgzR.TOCDigest() != tocDigest {
			return false, fmt.Errorf("layer contents fetched from registry doesn't match to the adverised TOC Digest")
		}
	}

	return true, nil
}

func VerifyImage(ctx context.Context, client *containerd.Client, ref string, resolver remotes.Resolver) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	refspec, err := reference.Parse(ref)
	if err != nil {
		return err
	}
	tmpdir, err := os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "verify")
	if err != nil {
		return err
	}
	defer func() {
		os.RemoveAll(tmpdir)
	}()
	verifier := NewVerifier(
		tmpdir,
		client,
		ns,
		func(reference.Spec) remotes.Resolver { return resolver },
		func(ctx context.Context, chainID digest.Digest) error {
			images, err := client.ListImages(ctx)
			if err != nil {
				return err
			}
			var targetImages []string
			for _, i := range images {
				diffIDs, err := i.RootFS(ctx)
				if err != nil {
					return err
				}
				if chainID == identity.ChainID(diffIDs) {
					targetImages = append(targetImages, i.Name())
				}
			}
			return fmt.Errorf("existing images contain invalid ChainIDs: %v", targetImages)
		},
	)
	manifest, imgConfig, err := verifier.FetchImageManifest(ctx, refspec)
	if err != nil {
		return err
	}
	diffIDs := imgConfig.RootFS.DiffIDs
	layers := manifest.Layers
	ok, err := verifier.Verify(ctx, diffIDs, layers)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("ChainID is invalid")
	}
	return nil
}
