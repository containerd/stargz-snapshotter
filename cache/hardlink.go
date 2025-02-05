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
		links:           make(map[string]*linkInfo),
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
	linkPath := BuildCachePath(hm.hlDir, key)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0700); err != nil {
		return fmt.Errorf("failed to create link dir: %w", err)
	}

	// Create temporary link path
	tmpLinkPath := BuildCachePath(hm.hlDir, key+".wip")
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
	// Mark as dirty for async persistence
	hm.dirty = true
	return nil
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
		return hm.persistLocked() // Use persistLocked since we already have the lock
	}
	return nil
}

// persistLocked persists link information while holding the lock
func (hm *HardlinkManager) persistLocked() error {
	if len(hm.links) == 0 {
		log.L.Debugf("No links to persist")
		return nil
	}

	linksFile := filepath.Join(hm.root, linksFileName)
	tmpFile := linksFile + ".tmp"

	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create temporary links file: %w", err)
	}

	defer f.Close()

	// Use more compact JSON encoding
	if err := json.NewEncoder(f).Encode(hm.links); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to encode links data: %w", err)
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

	log.L.Debugf("Persisted %d links", len(hm.links))
	return nil
}

// persist acquires lock and persists link information
func (hm *HardlinkManager) persist() error {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	return hm.persistLocked()
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

// persistWorker handles periodic persistence of link information
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

func (hm *HardlinkManager) Close() error {
	// Stop all background goroutines
	hm.persistTicker.Stop()
	hm.cleanupTicker.Stop()
	close(hm.persistDone)
	close(hm.cleanupDone)
	// Final persist of any remaining changes
	return hm.persist()
}
