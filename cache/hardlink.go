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

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/log"
)

const (
	hardlinkDirName = "hardlinks"
	linksFileName   = "links.json"
)

var (
	globalHLManager *HardlinkManager
	hlManagerOnce   sync.Once
	hlManagerMu     sync.RWMutex
)

// ChunkDigestMapping represents a mapping from chunkdigest to multiple keys
type ChunkDigestMapping struct {
	Digest string   `json:"digest"`
	Keys   []string `json:"keys"`
}

// HardlinkManager manages digest-to-file mappings and key-to-digest mappings
type HardlinkManager struct {
	root            string
	hlDir           string
	mu              sync.RWMutex
	digestToKeys    map[string]*ChunkDigestMapping // Maps chunkdigest to its associated keys
	keyToDigest     map[string]string              // Reverse index: maps key to its chunkdigest
	digestToFile    map[string]string              // Maps chunkdigest directly to file path
	cleanupInterval time.Duration
	// For batched persistence
	dirty         bool
	lastPersist   time.Time
	persistTicker *time.Ticker
	persistDone   chan struct{}
	cleanupDone   chan struct{} // Channel to signal cleanup goroutine to stop
	cleanupTicker *time.Ticker  // Ticker for cleanup
}

// NewHardlinkManager creates a new hardlink manager
func NewHardlinkManager(root string) (*HardlinkManager, error) {
	// Create hardlinks directory under root
	hlDir := filepath.Join(root, hardlinkDirName)
	if err := os.MkdirAll(hlDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create hardlink dir: %w", err)
	}

	hm := &HardlinkManager{
		root:            root,
		hlDir:           hlDir,
		digestToKeys:    make(map[string]*ChunkDigestMapping),
		keyToDigest:     make(map[string]string),
		digestToFile:    make(map[string]string),
		cleanupInterval: 24 * time.Hour,
		persistTicker:   time.NewTicker(5 * time.Second), // Batch writes every 5 seconds
		persistDone:     make(chan struct{}),
		cleanupDone:     make(chan struct{}),
		cleanupTicker:   time.NewTicker(24 * time.Hour),
	}

	// Restore persisted hardlink information
	if err := hm.restore(); err != nil {
		return nil, err
	}

	// Start periodic cleanup and persistence
	go hm.periodicCleanup()
	go hm.persistWorker()

	return hm, nil
}

// GetGlobalHardlinkManager returns the global hardlink manager instance
func GetGlobalHardlinkManager(root string) (*HardlinkManager, error) {
	hlManagerMu.RLock()
	if globalHLManager != nil {
		defer hlManagerMu.RUnlock()
		return globalHLManager, nil
	}
	hlManagerMu.RUnlock()

	hlManagerMu.Lock()
	defer hlManagerMu.Unlock()

	var initErr error
	hlManagerOnce.Do(func() {
		globalHLManager, initErr = NewHardlinkManager(root)
	})

	if initErr != nil {
		return nil, fmt.Errorf("failed to initialize global hardlink manager: %w", initErr)
	}

	return globalHLManager, nil
}

// GetLink gets the file path for a given digest
func (hm *HardlinkManager) GetLink(chunkdigest string) (string, bool) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	log.L.Debugf("Getting link for digest %q", chunkdigest)
	filePath, exists := hm.digestToFile[chunkdigest]
	if !exists {
		return "", false
	}

	// Verify the file still exists
	if _, err := os.Stat(filePath); err != nil {
		log.L.Debugf("File for digest %q no longer exists at %q: %v", chunkdigest, filePath, err)

		// We need to acquire a write lock
		hm.mu.RUnlock()
		hm.mu.Lock()
		defer hm.mu.Unlock()

		delete(hm.digestToFile, chunkdigest)
		hm.dirty = true
		return "", false
	}
	log.L.Debugf("Found link for digest %q: %q", chunkdigest, filePath)
	return filePath, true
}

// RegisterDigestFile registers a file as the primary source for a digest
func (hm *HardlinkManager) RegisterDigestFile(chunkdigest string, filePath string) error {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	// Verify the file exists
	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("file does not exist at %q: %w", filePath, err)
	}

	// Store the mapping
	hm.digestToFile[chunkdigest] = filePath
	log.L.Debugf("Registered file %q as primary source for digest %q", filePath, chunkdigest)

	// Mark as dirty for async persistence
	hm.dirty = true
	return nil
}

// MapKeyToDigest maps a key to a digest
func (hm *HardlinkManager) MapKeyToDigest(key string, chunkdigest string) error {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	// Check if the digest is registered
	if _, exists := hm.digestToFile[chunkdigest]; !exists {
		return fmt.Errorf("digest %q is not registered", chunkdigest)
	}

	// Update the mapping
	if oldDigest, exists := hm.keyToDigest[key]; exists {
		// Remove key from old digest mapping
		if mapping, ok := hm.digestToKeys[oldDigest]; ok {
			for i, k := range mapping.Keys {
				if k == key {
					mapping.Keys = append(mapping.Keys[:i], mapping.Keys[i+1:]...)
					break
				}
			}
			// If no more keys, remove the mapping
			if len(mapping.Keys) == 0 {
				delete(hm.digestToKeys, oldDigest)
			}
		}
	}

	// Get or create the mapping for this digest
	mapping, exists := hm.digestToKeys[chunkdigest]
	if !exists {
		mapping = &ChunkDigestMapping{
			Digest: chunkdigest,
			Keys:   make([]string, 0),
		}
		hm.digestToKeys[chunkdigest] = mapping
	}

	// Add key to mapping
	mapping.Keys = append(mapping.Keys, key)
	hm.keyToDigest[key] = chunkdigest

	// Mark as dirty for async persistence
	hm.dirty = true
	return nil
}

// cleanup removes unused digest mappings
func (hm *HardlinkManager) cleanup() error {
	// Use a timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Try to acquire lock with timeout
	lockChan := make(chan struct{})
	go func() {
		hm.mu.Lock()
		close(lockChan)
	}()

	select {
	case <-lockChan:
		defer hm.mu.Unlock()
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for lock")
	}

	// Find all digests that don't have any keys
	unusedDigests := make([]string, 0)
	for digest := range hm.digestToFile {
		mapping, exists := hm.digestToKeys[digest]
		if !exists || len(mapping.Keys) == 0 {
			unusedDigests = append(unusedDigests, digest)
		}
	}

	// Remove unused digests
	for _, digest := range unusedDigests {
		delete(hm.digestToFile, digest)
		delete(hm.digestToKeys, digest)
		log.L.Debugf("Removed unused digest: %q", digest)
	}

	if len(unusedDigests) > 0 {
		hm.dirty = true
		return hm.persistLocked()
	}

	return nil
}

// persistLocked persists digest information while holding the lock
func (hm *HardlinkManager) persistLocked() error {
	if len(hm.digestToKeys) == 0 && len(hm.digestToFile) == 0 {
		log.L.Debugf("No digest mappings to persist")
		return nil
	}

	linksFile := filepath.Join(hm.root, linksFileName)
	tmpFile := linksFile + ".tmp"

	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create temporary links file: %w", err)
	}

	defer f.Close()

	// Create a combined structure for persistence
	type persistData struct {
		DigestToKeys map[string]*ChunkDigestMapping `json:"digest_to_keys"`
		KeyToDigest  map[string]string              `json:"key_to_digest"`
		DigestToFile map[string]string              `json:"digest_to_file"`
	}

	data := persistData{
		DigestToKeys: hm.digestToKeys,
		KeyToDigest:  hm.keyToDigest,
		DigestToFile: hm.digestToFile,
	}

	if err := json.NewEncoder(f).Encode(data); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to encode digest data: %w", err)
	}

	if err := f.Sync(); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to sync links file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmpFile, linksFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to rename links file: %w", err)
	}

	log.L.Debugf("Persisted %d digest mappings and %d digest files",
		len(hm.digestToKeys), len(hm.digestToFile))
	return nil
}

// persist acquires lock and persists digest information
func (hm *HardlinkManager) persist() error {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	return hm.persistLocked()
}

// restore restores digest information from root directory
func (hm *HardlinkManager) restore() error {
	linksFile := filepath.Join(hm.root, linksFileName)

	f, err := os.Open(linksFile)
	if err != nil {
		if os.IsNotExist(err) {
			log.L.Debugf("No existing links file found at %s", linksFile)
			return nil
		}
		return fmt.Errorf("failed to open links file: %w", err)
	}
	defer f.Close()

	// Create a temporary structure for restoration
	type persistData struct {
		DigestToKeys map[string]*ChunkDigestMapping `json:"digest_to_keys"`
		KeyToDigest  map[string]string              `json:"key_to_digest"`
		DigestToFile map[string]string              `json:"digest_to_file"`
	}

	var data persistData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return fmt.Errorf("failed to decode links file: %w", err)
	}

	// Validate digest file mappings
	validDigestFiles := make(map[string]string)
	for digest, filePath := range data.DigestToFile {
		if _, err := os.Stat(filePath); err != nil {
			log.L.Debugf("Skipping invalid digest file mapping %q: file missing: %v", digest, err)
			continue
		}
		validDigestFiles[digest] = filePath
	}

	// Clean up key mappings for invalid digests
	validDigestToKeys := make(map[string]*ChunkDigestMapping)
	validKeyToDigest := make(map[string]string)

	for digest, mapping := range data.DigestToKeys {
		if _, exists := validDigestFiles[digest]; exists {
			validDigestToKeys[digest] = mapping

			// Validate keys for this digest
			for _, key := range mapping.Keys {
				validKeyToDigest[key] = digest
			}
		}
	}

	// Update mappings
	hm.digestToKeys = validDigestToKeys
	hm.keyToDigest = validKeyToDigest
	hm.digestToFile = validDigestFiles

	log.L.Debugf("Successfully restored %d digest mappings and %d digest files from %s",
		len(validDigestToKeys), len(validDigestFiles), linksFile)

	return nil
}

// periodicCleanup performs periodic cleanup
func (hm *HardlinkManager) periodicCleanup() {
	for {
		select {
		case <-hm.cleanupTicker.C:
			if err := hm.cleanup(); err != nil {
				log.L.Warnf("Failed to cleanup hardlinks: %v", err)
			}
		case <-hm.cleanupDone:
			return
		}
	}
}

// persistWorker handles periodic persistence of digest information
func (hm *HardlinkManager) persistWorker() {
	for {
		select {
		case <-hm.persistTicker.C:
			hm.mu.Lock()
			if hm.dirty && time.Since(hm.lastPersist) > 5*time.Second {
				if err := hm.persistLocked(); err != nil {
					log.L.Warnf("Failed to persist hardlink info: %v", err)
				}
				hm.dirty = false
				hm.lastPersist = time.Now()
			}
			hm.mu.Unlock()
		case <-hm.persistDone:
			return
		}
	}
}

// Close stops background goroutines and persists data
func (hm *HardlinkManager) Close() error {
	// Stop all background goroutines
	hm.persistTicker.Stop()
	hm.cleanupTicker.Stop()
	close(hm.persistDone)
	close(hm.cleanupDone)
	// Final persist of any remaining changes
	return hm.persist()
}

// HardlinkCapability represents a cache that supports hardlinking
type HardlinkCapability interface {
	// RegisterDigestFile registers a file as the primary source for a digest
	RegisterDigestFile(chunkDigest string, filepath string) error
	// GetLink returns the file path for a given digest
	GetLink(chunkDigest string) (string, bool)
	// MapKeyToDigest maps a key to a digest
	MapKeyToDigest(key string, chunkDigest string) error
	// CreateLink attempts to create a hardlink from an existing digest file to a key path
	CreateLink(key string, chunkDigest string, targetPath string) bool
	// IsEnabled returns whether hardlinking is enabled
	IsEnabled() bool
	// ProcessCacheGet handles hardlink-related logic for cache get operations
	ProcessCacheGet(key string, chunkDigest string, direct bool) (string, bool)
	// ProcessCacheAdd handles hardlink-related logic for cache add operations
	ProcessCacheAdd(key string, chunkDigest string, targetPath string) bool
}

// CreateLink attempts to create a hardlink from an existing digest file to a key path
// Returns true if successful, false otherwise
func (hm *HardlinkManager) CreateLink(key string, chunkdigest string, targetPath string) bool {
	// Try to get existing file for this digest
	if digestPath, exists := hm.GetLink(chunkdigest); exists {
		// Skip if source and target paths are the same
		if digestPath != targetPath {
			// Ensure target directory exists
			if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
				log.L.Debugf("Failed to create directory for hardlink: %v", err)
				return false
			}

			// Remove existing file if any
			_ = os.Remove(targetPath)

			// Create hardlink
			if err := os.Link(digestPath, targetPath); err != nil {
				log.L.Debugf("Failed to create hardlink from digest %q to key %q: %v",
					chunkdigest, key, err)
				return false
			}
			log.L.Debugf("Created hardlink from digest %q to key %q", chunkdigest, key)
			return true
		}
	}
	return false
}

// GenerateInternalKey creates a consistent internal key for a directory and key combination
func (hm *HardlinkManager) GenerateInternalKey(directory, key string) string {
	internalKey := sha256.Sum256([]byte(fmt.Sprintf("%s-%s", directory, key)))
	return fmt.Sprintf("%x", internalKey)
}

// IsEnabled returns true if the hardlink manager is properly initialized
func (hm *HardlinkManager) IsEnabled() bool {
	return hm != nil
}

// InitializeHardlinkManager creates a hardlink manager for the given cache directory
// Returns the manager and a boolean indicating if hardlinking is enabled
func InitializeHardlinkManager(cacheDir string, enableHardlink bool) (*HardlinkManager, bool) {
	if !enableHardlink {
		log.L.Infof("Hardlink feature is disabled for cache directory: %q", cacheDir)
		return nil, false
	}

	// Get root directory for hardlink manager (../../)
	hlManager, err := GetGlobalHardlinkManager(cacheDir)
	if err != nil {
		log.L.Warnf("Failed to initialize hardlink manager: %v", err)
		return nil, false
	}
	log.L.Infof("Using global hardlink manager with root directory: %q", cacheDir)
	return hlManager, true
}

// ProcessCacheGet handles hardlink-related logic for cache get operations
// Returns filepath and whether the file exists
func (hm *HardlinkManager) ProcessCacheGet(key string, chunkDigest string, direct bool) (string, bool) {
	if !hm.IsEnabled() || chunkDigest == "" {
		return "", false
	}

	return hm.GetLink(chunkDigest)
}

// ProcessCacheAdd handles hardlink-related logic for cache add operations
// Returns true if a hardlink was created and no further writes are needed
func (hm *HardlinkManager) ProcessCacheAdd(key string, chunkDigest string, targetPath string) bool {
	if !hm.IsEnabled() || chunkDigest == "" {
		return false
	}

	// Try to create a hardlink from existing digest file
	if hm.CreateLink(key, chunkDigest, targetPath) {
		// Map key to digest
		internalKey := hm.GenerateInternalKey(filepath.Dir(filepath.Dir(targetPath)), key)
		if err := hm.MapKeyToDigest(internalKey, chunkDigest); err != nil {
			log.L.Warnf("Failed to map key to digest: %v", err)
		}
		return true
	}

	return false
}
