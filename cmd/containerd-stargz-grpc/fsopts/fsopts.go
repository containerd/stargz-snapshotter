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

package fsopts

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/containerd/log"
	dbmetadata "github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/db"
	ipfs "github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/ipfs"
	"github.com/containerd/stargz-snapshotter/fs"
	"github.com/containerd/stargz-snapshotter/metadata"
	memorymetadata "github.com/containerd/stargz-snapshotter/metadata/memory"
	bolt "go.etcd.io/bbolt"
)

type Config struct {
	EnableIpfs    bool
	MetadataStore string
	OpenBoltDB    func(string) (*bolt.DB, error)
}

const (
	memoryMetadataType = "memory"
	dbMetadataType     = "db"
)

func ConfigFsOpts(ctx context.Context, rootDir string, config *Config) ([]fs.Option, error) {
	fsOpts := []fs.Option{fs.WithMetricsLogLevel(log.InfoLevel)}

	if config.EnableIpfs {
		fsOpts = append(fsOpts, fs.WithResolveHandler("ipfs", new(ipfs.ResolveHandler)))
	}

	mt, pruneFn, err := getMetadataStore(rootDir, config)
	if err != nil {
		return nil, fmt.Errorf("failed to configure metadata store: %w", err)
	}
	fsOpts = append(fsOpts, fs.WithMetadataStore(mt))
	if pruneFn != nil {
		fsOpts = append(fsOpts, fs.WithMetadataPruneFunc(pruneFn))
	}

	return fsOpts, nil
}

func getMetadataStore(rootDir string, config *Config) (metadata.Store, func(context.Context, map[string]struct{}) error, error) {
	switch config.MetadataStore {
	case "", memoryMetadataType:
		return memorymetadata.NewReader, nil, nil
	case dbMetadataType:
		if config.OpenBoltDB == nil {
			return nil, nil, fmt.Errorf("bolt DB is not configured")
		}
		db, err := config.OpenBoltDB(filepath.Join(rootDir, "metadata.db"))
		if err != nil {
			return nil, nil, err
		}
		store := func(sr *io.SectionReader, opts ...metadata.Option) (metadata.Reader, error) {
			return dbmetadata.NewReader(db, sr, opts...)
		}
		pruneFn := func(ctx context.Context, keep map[string]struct{}) error {
			return dbmetadata.Prune(ctx, db, keep)
		}
		return store, pruneFn, nil
	default:
		return nil, nil, fmt.Errorf("unknown metadata store type: %v; must be %v or %v",
			config.MetadataStore, memoryMetadataType, dbMetadataType)
	}
}
