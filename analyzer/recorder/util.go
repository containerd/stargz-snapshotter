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
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func CopyRecordFromImage(ctx context.Context, client *containerd.Client, recordInRef, targetRef string, platform platforms.MatchComparer) (map[digest.Digest][]string, error) {
	ctx, done, err := client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	cs := client.ContentStore()
	is := client.ImageService()

	recordInImg, err := is.Get(ctx, recordInRef)
	if err != nil {
		return nil, err
	}
	targetImg, err := is.Get(ctx, targetRef)
	if err != nil {
		return nil, err
	}

	recordInManifestDescs, err := containerdutil.ManifestDescs(ctx, cs, recordInImg.Target, platform)
	if err != nil {
		return nil, err
	}
	targetManifestDescs, err := containerdutil.ManifestDescs(ctx, cs, targetImg.Target, platform)
	if err != nil {
		return nil, err
	}

	records := make(map[digest.Digest]digest.Digest)
	var recordsMu sync.Mutex
	eg, egCtx := errgroup.WithContext(ctx)
	for _, targetDesc := range targetManifestDescs {
		targetDesc := targetDesc
		eg.Go(func() error {
			recordInDesc := recordInManifestDescs[0]
			if targetDesc.Platform != nil {
				for _, inDesc := range recordInManifestDescs {
					if inDesc.Platform == nil || platforms.Only(*targetDesc.Platform).Match(*inDesc.Platform) {
						if _, err := cs.Info(ctx, inDesc.Digest); err == nil {
							recordInDesc = inDesc
						}
					}
				}
			}
			p, err := content.ReadBlob(egCtx, cs, targetDesc)
			if err != nil {
				return err
			}
			var manifest ocispec.Manifest
			if err := json.Unmarshal(p, &manifest); err != nil {
				return err
			}
			rec, err := imageRecorderFromManifest(egCtx, cs, targetDesc, manifest)
			if err != nil {
				return err
			}
			scanner := prioritizedFilesScanner{r: rec}
			if _, err := converter.DefaultIndexConvertFunc(scanner.scan, false, platforms.All)(egCtx, cs, recordInDesc); err != nil {
				return err
			}
			d, err := scanner.r.Commit(egCtx)
			if err != nil {
				return err
			}
			recordsMu.Lock()
			records[targetDesc.Digest] = d
			recordsMu.Unlock()
			return rec.Close()
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return recordsToPaths(ctx, cs, records)
}

func recordsToPaths(ctx context.Context, cs content.Store, records map[digest.Digest]digest.Digest) (map[digest.Digest][]string, error) {
	pathsPerLayer := make(map[digest.Digest][]string)
	for targetDgst, recordOut := range records {
		pFilesPerLayer, err := PrioritizedFilesFromRecord(ctx, cs, targetDgst, recordOut)
		if err != nil {
			return nil, err
		}
		for layerDgst, files := range pFilesPerLayer {
			if len(files) <= 0 {
				continue
			}
			pathsPerLayer[layerDgst] = files
		}
	}
	return pathsPerLayer, nil
}

type prioritizedFilesScanner struct {
	r *ImageRecorder
}

func (s *prioritizedFilesScanner) scan(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return nil, nil
	}
	defer ra.Close()
	r, err := estargz.Open(io.NewSectionReader(ra, 0, desc.Size), estargz.WithDecompressors(new(zstdchunked.Decompressor)))
	if err != nil {
		return nil, nil
	}
	var offset int64
	if _, ok := r.Lookup(estargz.NoPrefetchLandmark); ok {
		return nil, nil // no prioritized files
	} else if e, ok := r.Lookup(estargz.PrefetchLandmark); ok {
		offset = e.Offset
	} else {
		return nil, nil // no prioritized files
	}
	if offset <= 0 {
		return nil, nil // no prioritized files
	}
	r.ForEachEntry(func(e *estargz.TOCEntry) bool {
		if e.Offset <= offset && e.Size > 0 {
			if err := s.r.Record(e.Name); err != nil {
				logrus.Debugf("failed to record %q: %v", e.Name, err)
			}
		}
		return true
	})
	return nil, nil
}

func PrioritizedFilesFromPaths(ctx context.Context, client *containerd.Client, paths []string, targetRef string, platform platforms.MatchComparer) (map[digest.Digest][]string, error) {
	cs := client.ContentStore()
	is := client.ImageService()

	targetImg, err := is.Get(ctx, targetRef)
	if err != nil {
		return nil, err
	}
	targetManifestDescs, err := containerdutil.ManifestDescs(ctx, cs, targetImg.Target, platform)
	if err != nil {
		return nil, err
	}
	records := make(map[digest.Digest]digest.Digest)
	var recordsMu sync.Mutex
	eg, egCtx := errgroup.WithContext(ctx)
	for _, targetDesc := range targetManifestDescs {
		targetDesc := targetDesc
		eg.Go(func() error {
			p, err := content.ReadBlob(egCtx, cs, targetDesc)
			if err != nil {
				return err
			}
			var manifest ocispec.Manifest
			if err := json.Unmarshal(p, &manifest); err != nil {
				return err
			}
			rec, err := imageRecorderFromManifest(egCtx, cs, targetDesc, manifest)
			if err != nil {
				return err
			}
			for _, p := range paths {
				if err := rec.Record(p); err != nil {
					logrus.Debugf("failed to record %q: %v", p, err)
				}
			}
			d, err := rec.Commit(egCtx)
			if err != nil {
				return err
			}
			recordsMu.Lock()
			records[targetDesc.Digest] = d
			recordsMu.Unlock()
			return rec.Close()
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return recordsToPaths(ctx, cs, records)
}

func PrioritizedFilesFromRecord(ctx context.Context, cs content.Store, manifestDgst, recordOutDgst digest.Digest) (map[digest.Digest][]string, error) {
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: recordOutDgst})
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	recordOutR := io.NewSectionReader(ra, 0, ra.Size())
	mb, err := content.ReadBlob(ctx, cs, ocispec.Descriptor{Digest: manifestDgst})
	if err != nil {
		return nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(mb, &manifest); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(recordOutR)
	pathsPerLayer := make(map[digest.Digest][]string, len(manifest.Layers))
	for _, layerDesc := range manifest.Layers {
		pathsPerLayer[layerDesc.Digest] = make([]string, 0)
	}
	added := make(map[digest.Digest]map[string]struct{}, len(manifest.Layers))
	for dec.More() {
		var e recorder.Entry
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		if *e.LayerIndex > len(manifest.Layers) || e.ManifestDigest != manifestDgst.String() {
			continue
		}
		dgst := manifest.Layers[*e.LayerIndex].Digest
		if _, ok := pathsPerLayer[dgst]; !ok {
			continue
		}
		if added[dgst] == nil {
			added[dgst] = map[string]struct{}{}
		}
		if _, ok := added[dgst][e.Path]; ok {
			continue
		}
		added[dgst][e.Path] = struct{}{}
		pathsPerLayer[dgst] = append(pathsPerLayer[dgst], e.Path)
	}

	return pathsPerLayer, nil
}
