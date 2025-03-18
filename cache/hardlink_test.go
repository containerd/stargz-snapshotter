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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Common test setup helper
func setupTestEnvironment(t *testing.T) (string, string, string, *HardlinkManager) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatalf("failed to create source dir: %v", err)
	}

	sourceFile := filepath.Join(sourceDir, "test.txt")
	content := []byte("test content")
	if err := os.WriteFile(sourceFile, content, 0600); err != nil {
		t.Fatalf("failed to create source file: %v", err)
	}

	hlm, err := NewHardlinkManager(tmpDir)
	if err != nil {
		t.Fatalf("failed to create hardlink manager: %v", err)
	}

	t.Cleanup(func() {
		if err := hlm.Close(); err != nil {
			t.Errorf("failed to close hardlink manager: %v", err)
		}
	})

	return tmpDir, sourceDir, sourceFile, hlm
}

// Test CreateLink functionality
func TestHardlinkManager_CreateLink(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)

	t.Run("Success", func(t *testing.T) {
		key := "test-key"
		if err := hlm.CreateLink(key, sourceFile); err != nil {
			t.Fatalf("CreateLink failed: %v", err)
		}

		// Verify link exists
		linkPath, exists := hlm.GetLink(key)
		if !exists {
			t.Fatal("link should exist")
		}

		// Verify hardlink
		sourceStat, err := os.Stat(sourceFile)
		if err != nil {
			t.Fatalf("failed to stat source: %v", err)
		}
		linkStat, err := os.Stat(linkPath)
		if err != nil {
			t.Fatalf("failed to stat link: %v", err)
		}
		if !os.SameFile(sourceStat, linkStat) {
			t.Fatal("should hardlink files")
		}
	})

	t.Run("NonExistentSource", func(t *testing.T) {
		err := hlm.CreateLink("bad-key", "nonexistent")
		if err == nil {
			t.Fatal("should fail with nonexistent source")
		}
	})
}

// Test GetLink functionality
func TestHardlinkManager_GetLink(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)

	t.Run("ExistingLink", func(t *testing.T) {
		key := "get-test"
		if err := hlm.CreateLink(key, sourceFile); err != nil {
			t.Fatalf("CreateLink failed: %v", err)
		}

		linkPath, exists := hlm.GetLink(key)
		if !exists {
			t.Fatal("link should exist")
		}
		if linkPath == "" {
			t.Fatal("link path should not be empty")
		}
	})

	t.Run("NonExistentLink", func(t *testing.T) {
		_, exists := hlm.GetLink("nonexistent")
		if exists {
			t.Fatal("should not find nonexistent link")
		}
	})
}

// Test cleanup functionality
func TestHardlinkManager_Cleanup(t *testing.T) {
	tmpDir, _, sourceFile, _ := setupTestEnvironment(t)

	cleanupDir := filepath.Join(tmpDir, "cleanup")
	cleanupHLM, err := NewHardlinkManager(cleanupDir)
	if err != nil {
		t.Fatalf("failed to create cleanup manager: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanupHLM.Close(); err != nil {
			t.Errorf("failed to close cleanup manager: %v", err)
		}
		os.RemoveAll(cleanupDir)
	})

	t.Run("ExpiredLink", func(t *testing.T) {
		key := "cleanup-test"
		if err := cleanupHLM.CreateLink(key, sourceFile); err != nil {
			t.Fatalf("CreateLink failed: %v", err)
		}

		// Force persist
		if err := cleanupHLM.persist(); err != nil {
			t.Fatalf("failed to persist: %v", err)
		}

		// Set link as expired
		cleanupHLM.mu.Lock()
		cleanupHLM.links[key].LastUsed = time.Now().Add(-31 * 24 * time.Hour)
		cleanupHLM.mu.Unlock()

		// Run cleanup with timeout
		done := make(chan error)
		go func() {
			done <- cleanupHLM.cleanup()
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("cleanup failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup timed out")
		}

		// Verify cleanup
		if _, exists := cleanupHLM.GetLink(key); exists {
			t.Fatal("expired link should be cleaned up")
		}
	})
}

// Test persistence and restoration
func TestHardlinkManager_PersistRestore(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)

	t.Run("PersistAndRestore", func(t *testing.T) {
		key := "persist-test"
		if err := hlm.CreateLink(key, sourceFile); err != nil {
			t.Fatalf("CreateLink failed: %v", err)
		}

		// Force persist
		if err := hlm.persist(); err != nil {
			t.Fatalf("failed to persist: %v", err)
		}
		time.Sleep(100 * time.Millisecond)

		// Create new manager
		hlm2, err := NewHardlinkManager(tmpDir)
		if err != nil {
			t.Fatalf("failed to create second manager: %v", err)
		}
		t.Cleanup(func() {
			if err := hlm2.Close(); err != nil {
				t.Errorf("failed to close second manager: %v", err)
			}
		})

		// Verify restoration
		linkPath, exists := hlm2.GetLink(key)
		if !exists {
			t.Fatal("link should be restored")
		}

		// Verify hardlink validity
		sourceStat, err := os.Stat(sourceFile)
		if err != nil {
			t.Fatalf("failed to stat source: %v", err)
		}
		linkStat, err := os.Stat(linkPath)
		if err != nil {
			t.Fatalf("failed to stat link: %v", err)
		}
		if !os.SameFile(sourceStat, linkStat) {
			t.Fatal("should hardlink restored files")
		}
	})
}

// Test concurrent operations
func TestHardlinkManager_Concurrent(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)

	done := make(chan bool)
	timeout := time.After(10 * time.Second)

	go func() {
		for i := 0; i < 100; i++ {
			hlm.GetLink("test-key")
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 100; i++ {
			hlm.CreateLink("test-key"+string(rune(i)), sourceFile)
		}
		done <- true
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
			continue
		case <-timeout:
			t.Fatal("concurrent test timed out")
		}
	}
}

// Test disabled hardlink functionality
func TestHardlinkManagerDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	dc, err := NewDirectoryCache(tmpDir, DirectoryCacheConfig{
		EnableHardlink: false,
	})
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	defer dc.Close()

	dirCache := dc.(*directoryCache)
	if dirCache.hlManager != nil {
		t.Fatal("hardlink manager should be nil when disabled")
	}

	t.Cleanup(func() {
		if err := dc.Close(); err != nil {
			t.Errorf("failed to close directory cache: %v", err)
		}
	})

	// Basic operations should work without hardlinks
	t.Run("BasicOperations", func(t *testing.T) {
		// Write test
		w, err := dc.Add("test-key")
		if err != nil {
			t.Fatalf("Add failed: %v", err)
		}
		if _, err := w.Write([]byte("test")); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		if err := w.Commit(); err != nil {
			t.Fatalf("Commit failed: %v", err)
		}
		w.Close()

		// Read test
		r, err := dc.Get("test-key")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
		defer r.Close()
	})
}
