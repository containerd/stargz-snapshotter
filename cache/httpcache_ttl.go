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
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/log"
)

var (
	cleanupJanitorMu     sync.Mutex
	globalCleanupJanitor *cleanupJanitor
)

type cleanupJanitor struct {
	rootDir string
	ttl     time.Duration

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// StartCleanupJanitor starts the process-wide janitor for httpcache cleanup.
func StartCleanupJanitor(rootDir string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}

	cleanupJanitorMu.Lock()
	defer cleanupJanitorMu.Unlock()

	if globalCleanupJanitor != nil {
		if globalCleanupJanitor.rootDir != rootDir || globalCleanupJanitor.ttl != ttl {
			log.L.WithFields(map[string]any{
				"root":          rootDir,
				"configured":    ttl,
				"existing_root": globalCleanupJanitor.rootDir,
				"existing_ttl":  globalCleanupJanitor.ttl,
			}).Warn("httpcache janitor already initialized; reusing existing janitor")
		}
		return
	}

	janitor := &cleanupJanitor{
		rootDir: rootDir,
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
	janitor.start()
	globalCleanupJanitor = janitor
}

func (j *cleanupJanitor) start() {
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()

		ticker := time.NewTicker(j.ttl)
		defer ticker.Stop()

		j.cleanupOnce()
		for {
			select {
			case <-ticker.C:
				j.cleanupOnce()
			case <-j.stopCh:
				return
			}
		}
	}()
}

func (j *cleanupJanitor) cleanupOnce() {
	if j.ttl <= 0 {
		return
	}

	cutoff := time.Now().Add(-j.ttl)
	_ = filepath.WalkDir(j.rootDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-j.stopCh:
			return fs.SkipAll
		default:
		}
		if d.IsDir() {
			if d.Name() == "wip" {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.L.WithError(err).Debugf("failed to remove expired cache entry %q", path)
		}
		return nil
	})
}
