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

func TestNewMemoryCacheWithTTL_Disabled(t *testing.T) {
	c := NewMemoryCacheWithTTL(0)
	if _, ok := c.(*MemoryCache); !ok {
		t.Fatalf("expected *MemoryCache when ttl is disabled; got %T", c)
	}
}

func TestNewMemoryCacheWithTTL_Expires(t *testing.T) {
	ttl := 30 * time.Millisecond
	c := NewMemoryCacheWithTTL(ttl)

	w, err := c.Add("k1")
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	_ = w.Close()

	r, err := c.Get("k1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	_ = r.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("entry did not expire within deadline")
		}
		time.Sleep(10 * time.Millisecond)
		if _, err := c.Get("k1"); err == nil {
			continue
		}
		break
	}
}

func TestDirectoryCacheCleanupOnce_RemovesExpiredFiles(t *testing.T) {
	base := t.TempDir()
	wip := filepath.Join(base, "wip")
	if err := os.MkdirAll(wip, 0700); err != nil {
		t.Fatalf("mkdir wip failed: %v", err)
	}

	dc := &directoryCache{
		directory:    base,
		wipDirectory: wip,
		entryTTL:     100 * time.Millisecond,
	}

	expired := filepath.Join(base, "aa", "expired")
	if err := os.MkdirAll(filepath.Dir(expired), 0700); err != nil {
		t.Fatalf("mkdir expired dir failed: %v", err)
	}
	if err := os.WriteFile(expired, []byte("x"), 0600); err != nil {
		t.Fatalf("write expired failed: %v", err)
	}
	old := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatalf("chtimes expired failed: %v", err)
	}

	fresh := filepath.Join(base, "bb", "fresh")
	if err := os.MkdirAll(filepath.Dir(fresh), 0700); err != nil {
		t.Fatalf("mkdir fresh dir failed: %v", err)
	}
	if err := os.WriteFile(fresh, []byte("y"), 0600); err != nil {
		t.Fatalf("write fresh failed: %v", err)
	}

	expiredWip := filepath.Join(wip, "tmp-expired")
	if err := os.WriteFile(expiredWip, []byte("z"), 0600); err != nil {
		t.Fatalf("write expired wip failed: %v", err)
	}
	if err := os.Chtimes(expiredWip, old, old); err != nil {
		t.Fatalf("chtimes expired wip failed: %v", err)
	}

	dc.cleanupOnce()

	if _, err := os.Stat(expired); err == nil {
		t.Fatalf("expected expired file to be removed")
	}
	if _, err := os.Stat(expiredWip); err == nil {
		t.Fatalf("expected expired wip file to be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("expected fresh file to remain; err=%v", err)
	}
}
