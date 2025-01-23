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

	"github.com/containerd/stargz-snapshotter/util/cacheutil"
)

var (
	globalHLManager *Manager
	hlManagerMu     sync.RWMutex
)

// SetGlobalManager sets the global Hardlink manager instance.
func SetGlobalManager(hm *Manager) {
	hlManagerMu.Lock()
	defer hlManagerMu.Unlock()
	globalHLManager = hm
}

// GetGlobalManager returns the global Hardlink manager instance (may be nil).
func GetGlobalManager() *Manager {
	hlManagerMu.RLock()
	defer hlManagerMu.RUnlock()
	return globalHLManager
}

// ChunkDigestMapping represents a mapping from chunkdigest to multiple keys
type ChunkDigestMapping struct {
	Digest string   `json:"digest"`
	Keys   []string `json:"keys"`
}

// Manager manages digest-to-file mappings and key-to-digest mappings
type Manager struct {
	mu           sync.RWMutex
	digestToKeys map[string]*ChunkDigestMapping
	keyToDigest  map[string]string
	digestToFile *cacheutil.LRUCache
}

// NewHardlinkManager creates a new hardlink manager
func NewHardlinkManager() (*Manager, error) {
	hm := &Manager{
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
	}

	return hm, nil
}

// GetLink gets the file path for a given digest.
// This returns the source file path that was registered through RegisterDigestFile.
// The returned file path can be used as a source for creating hardlinks via CreateLink.
func (hm *Manager) GetLink(chunkdigest string) (string, bool) {
	v, done, ok := hm.digestToFile.Get(chunkdigest)
	if !ok {
		return "", false
	}
	defer done()
	filePath, _ := v.(string)
	if _, err := os.Stat(filePath); err != nil {
		hm.digestToFile.Remove(chunkdigest)
		return "", false
	}
	return filePath, true
}

// RegisterDigestFile registers a file as the primary source for a digest.
// If directory and key are provided, it also maps the key to the digest.
func (hm *Manager) RegisterDigestFile(chunkdigest string, filePath string, directory string, key string) error {
	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("file does not exist at %q: %w", filePath, err)
	}
	_, done, _ := hm.digestToFile.Add(chunkdigest, filePath)
	done()

	// If directory and key are provided, map the key to the digest
	if directory != "" && key != "" {
		internalKey := hm.GenerateInternalKey(directory, key)
		if err := hm.MapKeyToDigest(internalKey, chunkdigest); err != nil {
			return fmt.Errorf("failed to map key to digest: %w", err)
		}
	}

	return nil
}

// MapKeyToDigest maps a key to a digest
func (hm *Manager) MapKeyToDigest(key string, chunkdigest string) error {
	_, done, ok := hm.digestToFile.Get(chunkdigest)
	if !ok {
		return fmt.Errorf("digest %q is not registered", chunkdigest)
	}
	done()

	hm.mu.Lock()
	defer hm.mu.Unlock()

	if oldDigest, exists := hm.keyToDigest[key]; exists {
		if mapping, ok := hm.digestToKeys[oldDigest]; ok {
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
	}

	mapping, exists := hm.digestToKeys[chunkdigest]
	if !exists {
		mapping = &ChunkDigestMapping{
			Digest: chunkdigest,
			Keys:   make([]string, 0),
		}
		hm.digestToKeys[chunkdigest] = mapping
	}

	mapping.Keys = append(mapping.Keys, key)
	hm.keyToDigest[key] = chunkdigest
	return nil
}

// CreateLink attempts to create a hardlink from an existing digest file to a key path.
// It uses the source file registered via RegisterDigestFile as the hardlink source.
// Returns nil if successful, error otherwise.
func (hm *Manager) CreateLink(key string, chunkdigest string, targetPath string) error {
	if digestPath, exists := hm.GetLink(chunkdigest); exists {
		if digestPath != targetPath {
			if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
				return fmt.Errorf("failed to create directory for hardlink: %w", err)
			}
			_ = os.Remove(targetPath)

			if err := os.Link(digestPath, targetPath); err != nil {
				return fmt.Errorf("failed to create hardlink from digest %q to key %q: %w", chunkdigest, key, err)
			}
			return nil
		}
	}
	return fmt.Errorf("no existing file found for digest %q or source and target paths are the same", chunkdigest)
}

// GenerateInternalKey creates a consistent internal key for a directory and key combination
func (hm *Manager) GenerateInternalKey(directory, key string) string {
	internalKey := sha256.Sum256([]byte(fmt.Sprintf("%s-%s", directory, key)))
	return fmt.Sprintf("%x", internalKey)
}

// IsEnabled returns true if the hardlink manager is properly initialized
func (hm *Manager) IsEnabled() bool {
	return hm != nil
}

// ProcessCacheGet handles hardlink-related logic for cache get operations
// Returns filepath and whether the file exists
func (hm *Manager) ProcessCacheGet(key string, chunkDigest string, direct bool) (string, bool) {
	if !hm.IsEnabled() || chunkDigest == "" {
		return "", false
	}
	return hm.GetLink(chunkDigest)
}

// ProcessCacheAdd handles hardlink-related logic for cache add operations
// Returns nil if a hardlink was created successfully, error otherwise
func (hm *Manager) ProcessCacheAdd(key string, chunkDigest string, targetPath string) error {
	if err := hm.CreateLink(key, chunkDigest, targetPath); err != nil {
		return fmt.Errorf("failed to create hardlink: %w", err)
	}
	internalKey := hm.GenerateInternalKey(filepath.Dir(filepath.Dir(targetPath)), key)
	if err := hm.MapKeyToDigest(internalKey, chunkDigest); err != nil {
		return fmt.Errorf("failed to map key to digest: %w", err)
	}
	return nil
}
