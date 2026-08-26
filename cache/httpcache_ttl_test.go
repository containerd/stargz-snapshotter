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

func TestStartCleanupJanitorInitializesOnce(t *testing.T) {
	stopCleanupJanitorForTest()
	t.Cleanup(stopCleanupJanitorForTest)

	root := t.TempDir()
	StartCleanupJanitor(root, time.Second)

	cleanupJanitorMu.Lock()
	first := globalCleanupJanitor
	cleanupJanitorMu.Unlock()
	if first == nil {
		t.Fatalf("expected janitor to be initialized")
	}

	otherRoot := t.TempDir()
	StartCleanupJanitor(otherRoot, 2*time.Second)

	cleanupJanitorMu.Lock()
	second := globalCleanupJanitor
	cleanupJanitorMu.Unlock()
	if second != first {
		t.Fatalf("expected existing janitor to be reused")
	}
}

func TestCleanupJanitorCleanupOnceRemovesExpiredFilesAcrossRoot(t *testing.T) {
	root := t.TempDir()
	digestDir := filepath.Join(root, "sha256-a")
	wipDir := filepath.Join(digestDir, "wip")
	if err := os.MkdirAll(wipDir, 0700); err != nil {
		t.Fatalf("mkdir wip failed: %v", err)
	}

	expired := filepath.Join(digestDir, "aa", "expired")
	if err := os.MkdirAll(filepath.Dir(expired), 0700); err != nil {
		t.Fatalf("mkdir expired dir failed: %v", err)
	}
	if err := os.WriteFile(expired, []byte("x"), 0600); err != nil {
		t.Fatalf("write expired failed: %v", err)
	}

	expiredWip := filepath.Join(wipDir, "tmp-expired")
	if err := os.WriteFile(expiredWip, []byte("y"), 0600); err != nil {
		t.Fatalf("write expired wip failed: %v", err)
	}

	fresh := filepath.Join(root, "sha256-b", "bb", "fresh")
	if err := os.MkdirAll(filepath.Dir(fresh), 0700); err != nil {
		t.Fatalf("mkdir fresh dir failed: %v", err)
	}
	if err := os.WriteFile(fresh, []byte("z"), 0600); err != nil {
		t.Fatalf("write fresh failed: %v", err)
	}

	old := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatalf("chtimes expired failed: %v", err)
	}
	if err := os.Chtimes(expiredWip, old, old); err != nil {
		t.Fatalf("chtimes expired wip failed: %v", err)
	}

	janitor := &cleanupJanitor{
		rootDir: root,
		ttl:     100 * time.Millisecond,
		stopCh:  make(chan struct{}),
	}
	janitor.cleanupOnce()

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expected expired file to be removed; err=%v", err)
	}
	if _, err := os.Stat(expiredWip); err != nil {
		t.Fatalf("expected wip file to remain; err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("expected fresh file to remain; err=%v", err)
	}
}

func stopCleanupJanitorForTest() {
	cleanupJanitorMu.Lock()
	janitor := globalCleanupJanitor
	globalCleanupJanitor = nil
	cleanupJanitorMu.Unlock()

	if janitor != nil {
		close(janitor.stopCh)
		janitor.wg.Wait()
	}
}
