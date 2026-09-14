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

package hardlink

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/containerd/stargz-snapshotter/util/cacheutil"
)

type ChunkDigestMapping struct {
	Digest string   `json:"digest"`
	Keys   []string `json:"keys"`
}

type Manager struct {
	mu           sync.RWMutex
	hardlinkRoot string
	digestToKeys map[string]*ChunkDigestMapping
	keyToDigest  map[string]string
	digestToFile *cacheutil.LRUCache
}

func New(root string) (*Manager, error) {
	hardlinkRoot := filepath.Join(root, "hardlinks")
	if err := os.MkdirAll(hardlinkRoot, 0700); err != nil {
		return nil, fmt.Errorf("failed to create hardlink directory: %w", err)
	}

	hm := &Manager{
		hardlinkRoot: hardlinkRoot,
		digestToKeys: make(map[string]*ChunkDigestMapping),
		keyToDigest:  make(map[string]string),
	}

	hm.digestToFile = cacheutil.NewLRUCache(100000)
	hm.digestToFile.OnEvicted = func(d string, v interface{}) {
		hm.mu.Lock()
		defer hm.mu.Unlock()
		if mapping, ok := hm.digestToKeys[d]; ok {
			for _, k := range mapping.Keys {
				delete(hm.keyToDigest, k)
			}
			delete(hm.digestToKeys, d)
		}
		if canonicalPath, ok := v.(string); ok {
			hm.cleanup(canonicalPath)
		}
	}

	return hm, nil
}

func (hm *Manager) cleanup(canonicalPath string) {
	stat, err := os.Stat(canonicalPath)
	if err != nil {
		return
	}

	if sys, ok := stat.Sys().(*unix.Stat_t); ok {
		if sys.Nlink == 1 {
			_ = os.Remove(canonicalPath)
		}
	}
}

func (hm *Manager) getLink(chunkdigest string) (string, bool) {
	v, done, ok := hm.digestToFile.Get(chunkdigest)
	if !ok {
		return "", false
	}
	defer done()
	canonicalPath, _ := v.(string)
	if _, err := os.Stat(canonicalPath); err != nil {
		hm.digestToFile.Remove(chunkdigest)
		return "", false
	}
	return canonicalPath, true
}

func (hm *Manager) Enroll(chunkdigest string, filePath string, directory string, key string) error {
	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("file does not exist at %q", filePath)
	}

	canonicalPath := filepath.Join(hm.hardlinkRoot, chunkdigest)
	if _, err := os.Stat(canonicalPath); os.IsNotExist(err) {
		if err := os.Link(filePath, canonicalPath); err != nil {
			return fmt.Errorf("failed to create canonical hardlink: %w", err)
		}
	}

	_, done, _ := hm.digestToFile.Add(chunkdigest, canonicalPath)
	done()

	if directory != "" && key != "" {
		internalKey := hm.generateInternalKey(directory, key)
		if err := hm.mapKeyToDigest(internalKey, chunkdigest); err != nil {
			return fmt.Errorf("failed to map key to digest: %w", err)
		}
	}

	return nil
}

func (hm *Manager) mapKeyToDigest(key string, chunkdigest string) error {
	_, done, ok := hm.digestToFile.Get(chunkdigest)
	if !ok {
		return fmt.Errorf("digest %q is not registered", chunkdigest)
	}
	done()

	hm.mu.Lock()
	defer hm.mu.Unlock()

	hm.removeKeyFromOldDigest(key)
	hm.addKeyToDigest(key, chunkdigest)
	return nil
}

func (hm *Manager) removeKeyFromOldDigest(key string) {
	oldDigest, exists := hm.keyToDigest[key]
	if !exists {
		return
	}

	mapping, ok := hm.digestToKeys[oldDigest]
	if !ok {
		return
	}

	for i, k := range mapping.Keys {
		if k == key {
			mapping.Keys = append(mapping.Keys[:i], mapping.Keys[i+1:]...)
			break
		}
	}

	if len(mapping.Keys) == 0 {
		delete(hm.digestToKeys, oldDigest)
	}
}

func (hm *Manager) addKeyToDigest(key, chunkdigest string) {
	mapping, exists := hm.digestToKeys[chunkdigest]
	if !exists {
		mapping = &ChunkDigestMapping{
			Digest: chunkdigest,
			Keys:   []string{},
		}
		hm.digestToKeys[chunkdigest] = mapping
	}
	mapping.Keys = append(mapping.Keys, key)
	hm.keyToDigest[key] = chunkdigest
}

func (hm *Manager) createLink(chunkdigest string, targetPath string) error {
	digestPath, exists := hm.getLink(chunkdigest)
	if !exists {
		return fmt.Errorf("no existing file found for digest %q", chunkdigest)
	}

	if digestPath == targetPath {
		return fmt.Errorf("source and target paths are the same")
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
		return fmt.Errorf("failed to create directory for hardlink: %w", err)
	}

	_ = os.Remove(targetPath)

	if err := os.Link(digestPath, targetPath); err != nil {
		return fmt.Errorf("failed to create hardlink: %w", err)
	}

	return nil
}

func (hm *Manager) generateInternalKey(directory, key string) string {
	internalKey := sha256.Sum256([]byte(fmt.Sprintf("%s-%s", directory, key)))
	return fmt.Sprintf("%x", internalKey)
}

func (hm *Manager) Get(key string, chunkDigest string, direct bool) (string, bool) {
	if hm == nil || chunkDigest == "" {
		return "", false
	}
	return hm.getLink(chunkDigest)
}

func (hm *Manager) Add(key string, chunkDigest string, targetPath string) error {
	if err := hm.createLink(chunkDigest, targetPath); err != nil {
		return err
	}
	internalKey := hm.generateInternalKey(filepath.Dir(filepath.Dir(targetPath)), key)
	return hm.mapKeyToDigest(internalKey, chunkDigest)
}
