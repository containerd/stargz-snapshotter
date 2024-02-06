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

package recorder

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"
)

const (
	whiteoutPrefix    = ".wh."
	whiteoutOpaqueDir = whiteoutPrefix + whiteoutPrefix + ".opq"
)

// ImageRecorder is a wrapper of recorder.Recroder. This holds the relationship
// between files and layer index in the specified image. So the client can record
// files without knowing about which layer this file belongs to.
type ImageRecorder struct {
	r              *recorder.Recorder
	index          []map[string]struct{}
	manifestDigest digest.Digest
	recordW        content.Writer
	recordWMu      sync.Mutex
}

func NewImageRecorder(ctx context.Context, cs content.Store, img images.Image, platformMC platforms.MatchComparer) (*ImageRecorder, error) {
	manifestDesc, err := containerdutil.ManifestDesc(ctx, cs, img.Target, platformMC)
	if err != nil {
		return nil, err
	}
	p, err := content.ReadBlob(ctx, cs, manifestDesc)
	if err != nil {
		return nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(p, &manifest); err != nil {
		return nil, err
	}
	return imageRecorderFromManifest(ctx, cs, manifestDesc, manifest)
}

func imageRecorderFromManifest(ctx context.Context, cs content.Store, manifestDesc ocispec.Descriptor, manifest ocispec.Manifest) (*ImageRecorder, error) {
	var eg errgroup.Group
	filesMap := make([]map[string]struct{}, len(manifest.Layers))
	for i, desc := range manifest.Layers {
		i, desc := i, desc
		filesMap[i] = make(map[string]struct{})

		// Create the index from the layer blob.
		// TODO: During optimization, we uncompress the blob several times (here and during
		//       creating eStargz layer). We should unify this process for better optimization
		//       performance.
		log.G(ctx).Infof("analyzing blob %q", desc.Digest)
		readerAt, err := cs.ReaderAt(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("failed to get reader blob %v: %w", desc.Digest, err)
		}
		defer readerAt.Close()
		r := io.Reader(io.NewSectionReader(readerAt, 0, desc.Size))
		if !uncompress.IsUncompressedType(desc.MediaType) {
			r, err = compression.DecompressStream(r)
			if err != nil {
				return nil, fmt.Errorf("cannot decompress layer %v: %w", desc.Digest, err)
			}
		}
		eg.Go(func() error {
			tr := tar.NewReader(r)
			for {
				h, err := tr.Next()
				if err != nil {
					if err == io.EOF {
						break
					}
					return err
				}
				filesMap[i][cleanEntryName(h.Name)] = struct{}{}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	recordW, err := content.OpenWriter(ctx, cs,
		content.WithRef(fmt.Sprintf("recorder-%v", xid.New().String())))
	if err != nil {
		return nil, fmt.Errorf("failed to open writer for recorder: %w", err)
	}
	return &ImageRecorder{
		r:              recorder.New(recordW),
		index:          filesMap,
		recordW:        recordW,
		manifestDigest: manifestDesc.Digest,
	}, nil
}

func (r *ImageRecorder) Record(name string) error {
	if name == "" {
		return nil
	}
	r.recordWMu.Lock()
	defer r.recordWMu.Unlock()

	name = cleanEntryName(name)
	index := -1
	for i := len(r.index) - 1; i >= 0; i-- {
		if _, ok := r.index[i][name]; ok {
			index = i
			break
		}

		// If this is deleted by a whiteout file or directory, return error.
		wh := cleanEntryName(path.Join(path.Dir("/"+name), whiteoutPrefix+path.Base("/"+name)))
		if _, ok := r.index[i][wh]; ok {
			return fmt.Errorf("%q is a deleted file", name)
		}
		whDir := cleanEntryName(path.Join(path.Dir("/"+name), whiteoutOpaqueDir))
		if _, ok := r.index[i][whDir]; ok {
			return fmt.Errorf("Parent dir of %q is a deleted directory", name)
		}
	}
	if index < 0 {
		return fmt.Errorf("file %q not found in index", name)
	}
	return r.r.Record(&recorder.Entry{
		Path:           name,
		ManifestDigest: r.manifestDigest.String(),
		LayerIndex:     &index,
	})
}

func (r *ImageRecorder) Commit(ctx context.Context) (digest.Digest, error) {
	r.recordWMu.Lock()
	defer r.recordWMu.Unlock()

	if err := r.recordW.Commit(ctx, 0, ""); err != nil && !errdefs.IsAlreadyExists(err) {
		return "", err
	}
	return r.recordW.Digest(), nil
}

func (r *ImageRecorder) Close() error {
	r.recordWMu.Lock()
	defer r.recordWMu.Unlock()

	return r.recordW.Close()
}

func cleanEntryName(name string) string {
	// Use path.Clean to consistently deal with path separators across platforms.
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}
