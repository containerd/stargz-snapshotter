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

	hlm, err := New(tmpDir)
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

func TestManager_RegisterAndGetDigest(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("RegisterAndRetrieveDigest", func(t *testing.T) {
		if err := hlm.Enroll(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("Enroll failed: %v", err)
		}
		canonicalPath, exists := hlm.Get("test-key", testDigest, false)
		if !exists {
			t.Fatal("digest should exist")
		}
		expectedCanonicalPath := filepath.Join(tmpDir, "hardlinks", testDigest)
		if canonicalPath != expectedCanonicalPath {
			t.Fatalf("expected canonical path %q, got %q", expectedCanonicalPath, canonicalPath)
		}
		content, err := os.ReadFile(canonicalPath)
		if err != nil {
			t.Fatalf("failed to read canonical file: %v", err)
		}
		if string(content) != "test content" {
			t.Fatalf("canonical file has wrong content: %q", string(content))
		}
	})

	t.Run("NonExistentFile", func(t *testing.T) {
		nonExistentFile := filepath.Join(t.TempDir(), "nonexistent.txt")
		nonexistentDigest := "sha256:nonexistent"
		err := hlm.Enroll(nonexistentDigest, nonExistentFile, "", "")
		if err == nil {
			t.Fatal("should fail with nonexistent file")
		}
	})

	t.Run("NonExistentDigest", func(t *testing.T) {
		nonexistentDigest := "sha256:nonexistent"
		_, exists := hlm.Get("test-key", nonexistentDigest, false)
		if exists {
			t.Fatal("should not find nonexistent digest")
		}
	})
}

func TestManager_Add(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("AddAndRetrieve", func(t *testing.T) {
		if err := hlm.Enroll(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("Enroll failed: %v", err)
		}

		targetPath := filepath.Join(tmpDir, "cache", "layer1", "chunk1")
		if err := hlm.Add("chunk1", testDigest, targetPath); err != nil {
			t.Fatalf("Add failed: %v", err)
		}

		if _, err := os.Stat(targetPath); err != nil {
			t.Fatalf("hardlink file should exist: %v", err)
		}

		content, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("failed to read hardlink file: %v", err)
		}
		if string(content) != "test content" {
			t.Fatalf("hardlink file has wrong content: %q", string(content))
		}
	})

	t.Run("NonExistentDigest", func(t *testing.T) {
		nonexistentDigest := "sha256:nonexistent"
		targetPath := filepath.Join(tmpDir, "cache", "layer2", "chunk1")
		err := hlm.Add("chunk1", nonexistentDigest, targetPath)
		if err == nil {
			t.Fatal("should fail with nonexistent digest")
		}
	})

	t.Run("Remapping", func(t *testing.T) {
		anotherFile := filepath.Join(t.TempDir(), "another.txt")
		anotherContent := []byte("different content")
		if err := os.WriteFile(anotherFile, anotherContent, 0600); err != nil {
			t.Fatalf("failed to create another file: %v", err)
		}
		anotherDigest := generateTestDigest("different content")
		if err := hlm.Enroll(anotherDigest, anotherFile, "", ""); err != nil {
			t.Fatalf("Enroll failed for another digest: %v", err)
		}

		targetPath1 := filepath.Join(tmpDir, "cache", "layer3", "chunk1")
		if err := hlm.Add("chunk1", testDigest, targetPath1); err != nil {
			t.Fatalf("Add failed for first digest: %v", err)
		}

		targetPath2 := filepath.Join(tmpDir, "cache", "layer4", "chunk1")
		if err := hlm.Add("chunk1", anotherDigest, targetPath2); err != nil {
			t.Fatalf("Add failed for remapping: %v", err)
		}

		canonicalPath, exists := hlm.Get("chunk1", anotherDigest, false)
		if !exists {
			t.Fatal("second digest should exist")
		}
		expectedCanonicalPath := filepath.Join(tmpDir, "hardlinks", anotherDigest)
		if canonicalPath != expectedCanonicalPath {
			t.Fatalf("expected canonical path %q, got %q", expectedCanonicalPath, canonicalPath)
		}
	})
}

func TestManager_CreateLink(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("SuccessfulHardlink", func(t *testing.T) {
		if err := hlm.Enroll(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("Enroll failed: %v", err)
		}
		targetPath := filepath.Join(tmpDir, "hardlink-target.txt")
		if err := hlm.Add("hardlink-key", testDigest, targetPath); err != nil {
			t.Fatalf("Add should succeed: %v", err)
		}
		canonicalPath := filepath.Join(tmpDir, "hardlinks", testDigest)
		canonicalStat, err := os.Stat(canonicalPath)
		if err != nil {
			t.Fatalf("failed to stat canonical: %v", err)
		}
		targetStat, err := os.Stat(targetPath)
		if err != nil {
			t.Fatalf("failed to stat target: %v", err)
		}
		if !os.SameFile(canonicalStat, targetStat) {
			t.Fatal("files should reference the same inode")
		}
	})

	t.Run("SamePathHardlink", func(t *testing.T) {
		if err := hlm.Enroll(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("Enroll failed: %v", err)
		}
		canonicalPath := filepath.Join(tmpDir, "hardlinks", testDigest)
		if err := hlm.Add("same-path-key", testDigest, canonicalPath); err == nil {
			t.Fatal("Add to canonical path should return error")
		}
	})
}

func TestManager_InMemoryState(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")

	t.Run("StateAvailable", func(t *testing.T) {
		if err := hlm.Enroll(testDigest, sourceFile, "", ""); err != nil {
			t.Fatalf("Enroll failed: %v", err)
		}
		targetPath := filepath.Join(tmpDir, "cache", "layer5", "chunk1")
		if err := hlm.Add("persist-test", testDigest, targetPath); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
		canonicalPath, exists := hlm.Get("persist-test", testDigest, false)
		if !exists {
			t.Fatal("digest mapping should exist")
		}
		expectedCanonicalPath := filepath.Join(tmpDir, "hardlinks", testDigest)
		if canonicalPath != expectedCanonicalPath {
			t.Fatalf("expected canonical path %q, got %q", expectedCanonicalPath, canonicalPath)
		}
	})
}

func TestManager_Concurrent(t *testing.T) {
	tmpDir, _, sourceFile, hlm := setupTestEnvironment(t)
	testDigest := generateTestDigest("test content")
	if err := hlm.Enroll(testDigest, sourceFile, "", ""); err != nil {
		t.Fatalf("Enroll failed: %v", err)
	}
	done := make(chan bool)
	timeout := time.After(10 * time.Second)
	go func() {
		for i := 0; i < 100; i++ {
			hlm.Get("test-key", testDigest, false)
		}
		done <- true
	}()
	go func() {
		for i := 0; i < 100; i++ {
			targetPath := filepath.Join(tmpDir, "cache", fmt.Sprintf("layer%d", i), "chunk1")
			_ = hlm.Add(fmt.Sprintf("test-key-%d", i), testDigest, targetPath)
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

func TestManager_UtilityFunctions(t *testing.T) {
	_, _, _, hlm := setupTestEnvironment(t)

	t.Run("Get_NilManager", func(t *testing.T) {
		var nilManager *Manager
		_, exists := nilManager.Get("key", "digest", false)
		if exists {
			t.Fatal("nil manager should return false")
		}
	})

	t.Run("Get_EmptyDigest", func(t *testing.T) {
		_, exists := hlm.Get("key", "", false)
		if exists {
			t.Fatal("empty digest should return false")
		}
	})
}
