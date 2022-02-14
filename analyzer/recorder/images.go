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
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/opencontainers/go-digest"
	ocispecVersion "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const recordJSON = "stargz.record.json"

// RecordOutToImage writes the specified record out blob as an image.
func RecordOutToImage(ctx context.Context, client *containerd.Client, recordOutDgst digest.Digest, ref string) (*images.Image, error) {
	cs := client.ContentStore()
	is := client.ImageService()

	// Write blob
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: recordOutDgst})
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	recordSize := ra.Size()
	sr := io.NewSectionReader(ra, 0, recordSize)
	blobW, err := content.OpenWriter(ctx, cs, content.WithRef(fmt.Sprintf("recording-ref-%s", recordOutDgst)))
	if err != nil {
		return nil, err
	}
	defer blobW.Close()
	if err := blobW.Truncate(0); err != nil {
		return nil, err
	}
	zw := gzip.NewWriter(blobW)
	defer zw.Close()
	diffID := digest.Canonical.Digester()
	tw := tar.NewWriter(io.MultiWriter(zw, diffID.Hash()))
	if err := tw.WriteHeader(&tar.Header{
		Name:     recordJSON,
		Typeflag: tar.TypeReg,
		Size:     recordSize,
	}); err != nil {
		return nil, err
	}
	if _, err := io.CopyN(tw, sr, recordSize); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	blobLabels := map[string]string{
		labels.LabelUncompressed: diffID.Digest().String(),
	}
	if err := blobW.Commit(ctx, 0, "", content.WithLabels(blobLabels)); err != nil && !errdefs.IsAlreadyExists(err) {
		return nil, err
	}
	blobInfo, err := cs.Info(ctx, blobW.Digest())
	if err != nil {
		return nil, err
	}
	blobDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    blobInfo.Digest,
		Size:      blobInfo.Size,
	}
	if err := blobW.Close(); err != nil {
		return nil, err
	}

	// Write config
	configW, err := content.OpenWriter(ctx, cs, content.WithRef(fmt.Sprintf("recording-ref-config-%s", recordOutDgst)))
	if err != nil {
		return nil, err
	}
	defer configW.Close()
	if err := json.NewEncoder(configW).Encode(ocispec.Image{
		Architecture: platforms.DefaultSpec().Architecture,
		OS:           platforms.DefaultSpec().OS,
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{diffID.Digest()},
		},
	}); err != nil {
		return nil, err
	}
	if err := configW.Commit(ctx, 0, ""); err != nil && !errdefs.IsAlreadyExists(err) {
		return nil, err
	}
	configInfo, err := cs.Info(ctx, configW.Digest())
	if err != nil {
		return nil, err
	}
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    configInfo.Digest,
		Size:      configInfo.Size,
	}
	if err := configW.Close(); err != nil {
		return nil, err
	}

	// Write manifest
	manifestW, err := content.OpenWriter(ctx, cs, content.WithRef(fmt.Sprintf("recording-ref-manifest-%s", recordOutDgst)))
	if err != nil {
		return nil, err
	}
	defer manifestW.Close()
	if err := json.NewEncoder(manifestW).Encode(ocispec.Manifest{
		Versioned: ocispecVersion.Versioned{
			SchemaVersion: 2,
		},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{blobDesc},
	}); err != nil {
		return nil, err
	}
	if err := manifestW.Commit(ctx, 0, "", content.WithLabels(map[string]string{
		"containerd.io/gc.ref.content.record.config": configDesc.Digest.String(),
		"containerd.io/gc.ref.content.record.blob":   blobDesc.Digest.String(),
	})); err != nil && !errdefs.IsAlreadyExists(err) {
		return nil, err
	}
	manifestInfo, err := cs.Info(ctx, manifestW.Digest())
	if err != nil {
		return nil, err
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    manifestInfo.Digest,
		Size:      manifestInfo.Size,
	}
	if err := manifestW.Close(); err != nil {
		return nil, err
	}

	// Write image
	_ = is.Delete(ctx, ref)
	res, err := is.Create(ctx, images.Image{
		Name:   ref,
		Target: manifestDesc,
	})
	return &res, err
}

// RecordInFromImage gets a record out file from the specified image.
func RecordInFromImage(ctx context.Context, client *containerd.Client, ref string, platform platforms.MatchComparer) (digest.Digest, error) {
	is := client.ImageService()
	cs := client.ContentStore()

	i, err := is.Get(ctx, ref)
	if err != nil {
		return "", err
	}

	manifestDesc, err := containerdutil.ManifestDesc(ctx, cs, i.Target, platform)
	if err != nil {
		return "", err
	}
	p, err := content.ReadBlob(ctx, cs, manifestDesc)
	if err != nil {
		return "", err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(p, &manifest); err != nil {
		return "", err
	}
	if len(manifest.Layers) != 1 {
		return "", fmt.Errorf("record image must have 1 layer")
	}
	recordOut := manifest.Layers[0]

	ra, err := cs.ReaderAt(ctx, recordOut)
	if err != nil {
		return "", err
	}
	defer ra.Close()
	dr, err := compression.DecompressStream(io.NewSectionReader(ra, 0, ra.Size()))
	if err != nil {
		return "", err
	}
	var recordOutR io.Reader
	var recordOutSize int64
	tr := tar.NewReader(dr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return "", err
			}
		}
		if cleanEntryName(h.Name) == recordJSON {
			recordOutR, recordOutSize = tr, h.Size
			break
		}
	}
	if recordOutR == nil {
		return "", fmt.Errorf("failed to find record file")
	}
	recordW, err := content.OpenWriter(ctx, cs, content.WithRef(fmt.Sprintf("recording-in-ref-%s", manifestDesc.Digest)))
	if err != nil {
		return "", err
	}
	defer recordW.Close()
	if err := recordW.Truncate(0); err != nil {
		return "", err
	}
	if _, err := io.CopyN(recordW, recordOutR, recordOutSize); err != nil {
		return "", err
	}
	if err := recordW.Commit(ctx, 0, ""); err != nil && !errdefs.IsAlreadyExists(err) {
		return "", err
	}
	dgst := recordW.Digest()
	if err := recordW.Close(); err != nil {
		return "", err
	}
	return dgst, nil
}
