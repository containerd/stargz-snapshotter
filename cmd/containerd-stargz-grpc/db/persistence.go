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

package db

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/externaltoc"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/containerd/stargz-snapshotter/metadata"
	digest "github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

var (
	// filesystem-level metadata (stored in the filesystem bucket)
	bucketKeyReady        = []byte("ready")
	bucketKeyRootID       = []byte("rootID")
	bucketKeyTOCDigest    = []byte("tocDigest")
	bucketKeyDecompressor = []byte("decompressor")
)

var initLocks sync.Map

func withInitLock(key string, fn func() (metadata.Reader, error)) (metadata.Reader, error) {
	muIface, _ := initLocks.LoadOrStore(key, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func decompressorKey(d metadata.Decompressor) string {
	switch d.(type) {
	case *estargz.GzipDecompressor:
		return "gzip"
	case *estargz.LegacyGzipDecompressor:
		return "legacy-gzip"
	case *zstdchunked.Decompressor:
		return "zstdchunked"
	case *externaltoc.GzipDecompressor:
		return "externaltoc-gzip"
	default:
		return ""
	}
}

func findDecompressorByKey(ds []metadata.Decompressor, key string) metadata.Decompressor {
	if key == "" {
		return nil
	}
	for _, d := range ds {
		if decompressorKey(d) == key {
			return d
		}
	}
	return nil
}

type persistedFSInfo struct {
	rootID          uint32
	tocDigest       digest.Digest
	decompressorKey string
}

func loadPersisted(db *bolt.DB, fsID string) (*persistedFSInfo, error) {
	var info *persistedFSInfo
	err := db.View(func(tx *bolt.Tx) error {
		filesystems := tx.Bucket(bucketKeyFilesystems)
		if filesystems == nil {
			return nil
		}
		lbkt := filesystems.Bucket([]byte(fsID))
		if lbkt == nil {
			return nil
		}
		if !decodeBool(lbkt.Get(bucketKeyReady)) {
			return nil
		}
		rootU, _ := binary.Uvarint(lbkt.Get(bucketKeyRootID))
		tocStr := string(lbkt.Get(bucketKeyTOCDigest))
		decompKey := string(lbkt.Get(bucketKeyDecompressor))
		if rootU == 0 || tocStr == "" || decompKey == "" {
			return nil
		}
		tocD, err := digest.Parse(tocStr)
		if err != nil {
			return nil
		}
		info = &persistedFSInfo{
			rootID:          uint32(rootU),
			tocDigest:       tocD,
			decompressorKey: decompKey,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

func decodeBool(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	v, _ := binary.Uvarint(b)
	return v != 0
}

func (r *reader) markReady(decompKey string) error {
	return r.db.Batch(func(tx *bolt.Tx) error {
		filesystems := tx.Bucket(bucketKeyFilesystems)
		if filesystems == nil {
			return fmt.Errorf("filesystems bucket not found")
		}
		lbkt := filesystems.Bucket([]byte(r.fsID))
		if lbkt == nil {
			return fmt.Errorf("filesystem bucket %q not found", r.fsID)
		}
		if err := lbkt.Put(bucketKeyTOCDigest, []byte(r.tocDigest.String())); err != nil {
			return err
		}
		if err := lbkt.Put(bucketKeyDecompressor, []byte(decompKey)); err != nil {
			return err
		}
		one, err := encodeUint(1)
		if err != nil {
			return err
		}
		return lbkt.Put(bucketKeyReady, one)
	})
}

// Prune removes filesystem buckets not included in keep.
// Keys of keep must be the same as the filesystem bucket keys (e.g. layer digest strings).
func Prune(_ context.Context, db *bolt.DB, keep map[string]struct{}) error {
	return db.Batch(func(tx *bolt.Tx) error {
		filesystems := tx.Bucket(bucketKeyFilesystems)
		if filesystems == nil {
			return nil
		}

		var delKeys [][]byte
		if err := filesystems.ForEach(func(k, v []byte) error {
			if v != nil {
				return nil
			}
			if _, ok := keep[string(k)]; ok {
				return nil
			}
			kk := make([]byte, len(k))
			copy(kk, k)
			delKeys = append(delKeys, kk)
			return nil
		}); err != nil {
			return err
		}

		for _, k := range delKeys {
			if err := filesystems.DeleteBucket(k); err != nil {
				return err
			}
		}
		return nil
	})
}
