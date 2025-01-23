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
	"testing"
	"time"
)

// getDigestForKey is a test helper function that returns the digest mapped to the given key, if any.
func (hm *Manager) getDigestForKey(key string) (string, bool) {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	d, ok := hm.keyToDigest[key]
	return d, ok
}

// Common test setup helper
func setupTestEnvironment(t *testing.T) (string, string, string, *Manager) {
	t.Helper()
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

	hlm, err := NewHardlinkManager()
	if err != nil {
		t.Fatalf("failed to create hardlink manager: %v", err)
	}

	return tmpDir, sourceDir, sourceFile, hlm
}

// Helper function to generate a digest for testing
func generateTestDigest(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", hash[:])
}

// Test RegisterDigestFile and GetLink functionality
func TestManager_RegisterAndGetDigest(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("RegisterAndRetrieveDigest", func(t *testing.T) {
		if err := hlm.RegisterDigestFile(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("RegisterDigestFile failed: %v", err)
		}
		filePath, exists := hlm.GetLink(testDigest)
		if !exists {
			t.Fatal("digest should exist")
		}
		if filePath != sourceFile {
			t.Fatalf("expected file path %q, got %q", sourceFile, filePath)
		}
	})

	t.Run("NonExistentFile", func(t *testing.T) {
		nonExistentFile := filepath.Join(t.TempDir(), "nonexistent.txt")
		nonexistentDigest := "sha256:nonexistent"
		err := hlm.RegisterDigestFile(nonexistentDigest, nonExistentFile, "", "")
		if err == nil {
			t.Fatal("should fail with nonexistent file")
		}
	})

	t.Run("NonExistentDigest", func(t *testing.T) {
		nonexistentDigest := "sha256:nonexistent"
		_, exists := hlm.GetLink(nonexistentDigest)
		if exists {
			t.Fatal("should not find nonexistent digest")
		}
	})
}

// Test MapKeyToDigest functionality
func TestManager_MapKeyToDigest(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("MapKeyAndGetDigest", func(t *testing.T) {
		if err := hlm.RegisterDigestFile(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("RegisterDigestFile failed: %v", err)
		}
		key := "test-key"
		if err := hlm.MapKeyToDigest(key, testDigest); err != nil {
			t.Fatalf("MapKeyToDigest failed: %v", err)
		}
		mappedDigest, exists := hlm.getDigestForKey(key)
		if !exists {
			t.Fatal("key should be mapped to digest")
		}
		if mappedDigest != testDigest {
			t.Fatalf("expected digest %q, got %q", testDigest, mappedDigest)
		}
	})

	t.Run("NonExistentDigest", func(t *testing.T) {
		nonexistentDigest := "sha256:nonexistent"
		err := hlm.MapKeyToDigest("bad-key", nonexistentDigest)
		if err == nil {
			t.Fatal("should fail with nonexistent digest")
		}
	})

	t.Run("RemappingKey", func(t *testing.T) {
		anotherFile := filepath.Join(t.TempDir(), "another.txt")
		anotherContent := []byte("different content")
		if err := os.WriteFile(anotherFile, anotherContent, 0600); err != nil {
			t.Fatalf("failed to create another file: %v", err)
		}
		anotherDigest := generateTestDigest("different content")
		if err := hlm.RegisterDigestFile(anotherDigest, anotherFile, "", ""); err != nil {
			t.Fatalf("RegisterDigestFile failed for another digest: %v", err)
		}
		key := "remap-key"
		if err := hlm.MapKeyToDigest(key, testDigest); err != nil {
			t.Fatalf("MapKeyToDigest failed for first digest: %v", err)
		}
		if _, exists := hlm.GetLink(testDigest); !exists {
			t.Fatal("first digest should exist")
		}
		if err := hlm.MapKeyToDigest(key, anotherDigest); err != nil {
			t.Fatalf("MapKeyToDigest failed for remapping: %v", err)
		}
		mappedDigest, exists := hlm.getDigestForKey(key)
		if !exists {
			t.Fatal("key should still be mapped")
		}
		if mappedDigest != anotherDigest {
			t.Fatalf("expected digest %q, got %q", anotherDigest, mappedDigest)
		}
		linkPath, exists := hlm.GetLink(anotherDigest)
		if !exists {
			t.Fatal("second digest should exist")
		}
		if linkPath != anotherFile {
			t.Fatalf("expected file path %q, got %q", anotherFile, linkPath)
		}
	})
}

// Test CreateLink functionality
func TestManager_CreateLink(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("SuccessfulHardlink", func(t *testing.T) {
		if err := hlm.RegisterDigestFile(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("RegisterDigestFile failed: %v", err)
		}
		targetPath := filepath.Join(tmpDir, "hardlink-target.txt")
		if err := hlm.CreateLink("hardlink-key", testDigest, targetPath); err != nil {
			t.Fatalf("CreateLink should succeed: %v", err)
		}
		sourceStat, err := os.Stat(sourceFile)
		if err != nil {
			t.Fatalf("failed to stat source: %v", err)
		}
		targetStat, err := os.Stat(targetPath)
		if err != nil {
			t.Fatalf("failed to stat target: %v", err)
		}
		if !os.SameFile(sourceStat, targetStat) {
			t.Fatal("files should reference the same inode")
		}
	})

	t.Run("SamePathHardlink", func(t *testing.T) {
		if err := hlm.RegisterDigestFile(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("RegisterDigestFile failed: %v", err)
		}
		if err := hlm.CreateLink("same-path-key", testDigest, sourceFile); err == nil {
			t.Fatal("CreateLink to same path should return error")
		}
	})
}

// Test in-memory state (no persistence)
func TestManager_InMemoryState(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("StateAvailable", func(t *testing.T) {
		if err := hlm.RegisterDigestFile(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("RegisterDigestFile failed: %v", err)
		}
		key := "persist-test"
		if err := hlm.MapKeyToDigest(key, testDigest); err != nil {
			t.Fatalf("MapKeyToDigest failed: %v", err)
		}
		digest, exists := hlm.getDigestForKey(key)
		if !exists {
			t.Fatal("key mapping should exist")
		}
		if digest != testDigest {
			t.Fatalf("expected digest %q, got %q", testDigest, digest)
		}
		digestPath, exists := hlm.GetLink(testDigest)
		if !exists {
			t.Fatal("digest mapping should exist")
		}
		if digestPath != sourceFile {
			t.Fatalf("expected digest path %q, got %q", sourceFile, digestPath)
		}
	})
}

// Test concurrent operations
func TestManager_Concurrent(t *testing.T) {
	_, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")
	if err := hlm.RegisterDigestFile(testDigest, sourceFile, "", ""); err != nil {
		t.Fatalf("RegisterDigestFile failed: %v", err)
	}
	done := make(chan bool)
	timeout := time.After(10 * time.Second)
	go func() {
		for i := 0; i < 100; i++ {
			hlm.GetLink(testDigest)
		}
		done <- true
	}()
	go func() {
		for i := 0; i < 100; i++ {
			key := fmt.Sprintf("test-key-%d", i)
			hlm.MapKeyToDigest(key, testDigest)
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

// Test utility functions
func TestManager_UtilityFunctions(t *testing.T) {
	_, _, _, hlm := setupTestEnvironment(t)

	t.Run("GenerateInternalKey", func(t *testing.T) {
		key1 := hlm.GenerateInternalKey("/path/to/dir", "key1")
		key2 := hlm.GenerateInternalKey("/path/to/dir", "key2")
		if key1 == key2 {
			t.Fatal("different keys should generate different internal keys")
		}
		key1Again := hlm.GenerateInternalKey("/path/to/dir", "key1")
		if key1 != key1Again {
			t.Fatal("same inputs should generate same internal key")
		}
	})

	t.Run("IsEnabled", func(t *testing.T) {
		if !hlm.IsEnabled() {
			t.Fatal("hardlink manager should be enabled")
		}
		var nilManager *Manager
		if nilManager.IsEnabled() {
			t.Fatal("nil manager should report as disabled")
		}
	})
}
