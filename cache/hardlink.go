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

type linkInfo struct {
	SourcePath string    `json:"source"`
	LinkPath   string    `json:"link"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsed   time.Time `json:"last_used"`
}

// HardlinkManager manages creation, recovery and cleanup of hardlinks
type HardlinkManager struct {
	root            string
	hlDir           string
	mu              sync.RWMutex
	links           map[string]*linkInfo
	cleanupInterval time.Duration
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
		links:           make(map[string]*linkInfo),
		cleanupInterval: 24 * time.Hour,
	}

	// Restore persisted hardlink information
	if err := hm.restore(); err != nil {
		return nil, err
	}

	// Start periodic cleanup
	go hm.periodicCleanup()

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

// CreateLink creates a hardlink with retry logic
func (hm *HardlinkManager) CreateLink(key string, sourcePath string) error {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	// Add debug info about source file
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		log.L.Debugf("Source file stat failed %q: %v", sourcePath, err)
		return fmt.Errorf("failed to stat source file: %w", err)
	}

	// Create hardlink in hardlinks directory
	linkPath := filepath.Join(hm.hlDir, key[:2], key)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0700); err != nil {
		return fmt.Errorf("failed to create link dir: %w", err)
	}

	// Create temporary link path
	tmpLinkPath := linkPath + ".tmp"

	// Remove temporary link if it exists
	os.Remove(tmpLinkPath)

	// Retry logic for creating hardlink
	const maxRetries = 3
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := os.Link(sourcePath, tmpLinkPath); err != nil {
			lastErr = err
			if i < maxRetries-1 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return fmt.Errorf("failed to create temporary hardlink after %d attempts: %w", maxRetries, lastErr)
		}
		break
	}

	// Verify the temporary link
	tmpInfo, err := os.Stat(tmpLinkPath)
	if err != nil {
		os.Remove(tmpLinkPath)
		return fmt.Errorf("failed to stat temporary link: %w", err)
	}

	if !os.SameFile(sourceInfo, tmpInfo) {
		os.Remove(tmpLinkPath)
		return fmt.Errorf("temporary link verification failed - different inodes")
	}

	// Atomically rename temporary link to final location
	if err := os.Rename(tmpLinkPath, linkPath); err != nil {
		os.Remove(tmpLinkPath)
		return fmt.Errorf("failed to rename temporary link: %w", err)
	}

	// Record link info
	hm.links[key] = &linkInfo{
		SourcePath: sourcePath,
		LinkPath:   linkPath,
		CreatedAt:  time.Now(),
		LastUsed:   time.Now(),
	}

	// Persist link info
	return hm.persist()
}

// GetLink gets the hardlink path if it exists
func (hm *HardlinkManager) GetLink(key string) (string, bool) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()

	if info, exists := hm.links[key]; exists {
		// Check if link is still valid
		if _, err := os.Stat(info.LinkPath); err != nil {
			log.L.Debugf("Hardlink %q is no longer valid: %v", info.LinkPath, err)
			delete(hm.links, key)
			return "", false
		}
		// Update last used time
		info.LastUsed = time.Now()
		return info.LinkPath, true
	}
	return "", false
}

// cleanup cleans up expired hardlinks
func (hm *HardlinkManager) cleanup() error {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	now := time.Now()
	expiredKeys := make([]string, 0)

	// Find expired links
	for key, info := range hm.links {
		if now.Sub(info.LastUsed) > 30*24*time.Hour {
			expiredKeys = append(expiredKeys, key)
		}
	}

	// Remove expired links in batch
	for _, key := range expiredKeys {
		info := hm.links[key]
		if err := os.Remove(info.LinkPath); err != nil && !os.IsNotExist(err) {
			log.L.Warnf("Failed to remove expired hardlink %q: %v", info.LinkPath, err)
			continue
		}
		delete(hm.links, key)
	}

	if len(expiredKeys) > 0 {
		return hm.persist()
	}
	return nil
}

// persist persists link information to root directory
func (hm *HardlinkManager) persist() error {
	if len(hm.links) == 0 {
		log.L.Debugf("No links to persist")
		return nil
	}

	linksFile := filepath.Join(hm.root, linksFileName)

	// Create temporary file for atomic write
	tmpFile := linksFile + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create temporary links file: %w", err)
	}

	// Use a closure to handle file close and cleanup
	if err := func() error {
		defer f.Close()

		// Pretty print JSON for better readability
		encoder := json.NewEncoder(f)
		encoder.SetIndent("", "    ")
		if err := encoder.Encode(hm.links); err != nil {
			return fmt.Errorf("failed to encode links data: %w", err)
		}

		// Ensure data is written to disk
		if err := f.Sync(); err != nil {
			return fmt.Errorf("failed to sync links file: %w", err)
		}

		return nil
	}(); err != nil {
		os.Remove(tmpFile)
		return err
	}

	// Atomic rename
	if err := os.Rename(tmpFile, linksFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to rename links file: %w", err)
	}

	log.L.Debugf("Successfully persisted %d links to %s", len(hm.links), linksFile)
	return nil
}

// restore restores link information from root directory
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

	// Create a temporary map to avoid corrupting the existing one on error
	tempLinks := make(map[string]*linkInfo)
	if err := json.NewDecoder(f).Decode(&tempLinks); err != nil {
		return fmt.Errorf("failed to decode links file: %w", err)
	}

	// Validate restored links
	validLinks := make(map[string]*linkInfo)
	for key, info := range tempLinks {
		// Check if both source and link files exist
		if _, err := os.Stat(info.SourcePath); err != nil {
			log.L.Debugf("Skipping invalid link %q: source file missing: %v", key, err)
			continue
		}
		if _, err := os.Stat(info.LinkPath); err != nil {
			log.L.Debugf("Skipping invalid link %q: link file missing: %v", key, err)
			continue
		}
		validLinks[key] = info
	}

	// Update links map only after validation
	hm.links = validLinks
	log.L.Debugf("Successfully restored %d valid links from %s", len(validLinks), linksFile)

	return nil
}

// periodicCleanup performs periodic cleanup
func (hm *HardlinkManager) periodicCleanup() {
	ticker := time.NewTicker(hm.cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		if err := hm.cleanup(); err != nil {
			log.L.Warnf("Failed to cleanup hardlinks: %v", err)
		}
	}
}
