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
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/util/cacheutil"
)

func (dc *directoryCache) startCleanupIfNeeded() {
	if dc.entryTTL <= 0 {
		return
	}
	dc.closedMu.Lock()
	if dc.closed || dc.cleanupStopCh != nil {
		dc.closedMu.Unlock()
		return
	}
	stopCh := make(chan struct{})
	dc.cleanupStopCh = stopCh
	dc.cleanupWg.Add(1)
	dc.closedMu.Unlock()

	interval := dc.entryTTL
	go func() {
		defer dc.cleanupWg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		dc.cleanupOnce()
		for {
			select {
			case <-ticker.C:
				dc.cleanupOnce()
			case <-stopCh:
				return
			}
		}
	}()
}

func (dc *directoryCache) cleanupOnce() {
	if dc.entryTTL <= 0 {
		return
	}
	if dc.isClosed() {
		return
	}

	cutoff := time.Now().Add(-dc.entryTTL)
	wipBase := dc.wipDirectory

	_ = filepath.WalkDir(dc.directory, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if dc.isClosed() {
			return fs.SkipAll
		}
		if d.IsDir() {
			if path == wipBase {
				return fs.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.L.WithError(err).Debugf("failed to remove expired cache entry %q", path)
		}
		return nil
	})

	_ = filepath.WalkDir(wipBase, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if dc.isClosed() {
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		_ = os.Remove(path)
		return nil
	})
}

func NewMemoryCacheWithTTL(ttl time.Duration) BlobCache {
	if ttl <= 0 {
		return NewMemoryCache()
	}
	bufPool := &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	c := cacheutil.NewTTLCache(ttl)
	c.OnEvicted = func(key string, value interface{}) {
		b := value.(*bytes.Buffer)
		b.Reset()
		bufPool.Put(b)
	}
	return &ttlMemoryCache{
		c:       c,
		bufPool: bufPool,
	}
}

type ttlMemoryCache struct {
	c       *cacheutil.TTLCache
	bufPool *sync.Pool
}

func (mc *ttlMemoryCache) Get(key string, opts ...Option) (Reader, error) {
	v, done, ok := mc.c.Get(key)
	if !ok {
		return nil, fmt.Errorf("missed cache: %q", key)
	}
	b := v.(*bytes.Buffer)
	return &reader{
		ReaderAt: bytes.NewReader(b.Bytes()),
		closeFunc: func() error {
			done(false)
			return nil
		},
	}, nil
}

func (mc *ttlMemoryCache) Add(key string, opts ...Option) (Writer, error) {
	b := mc.bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return &writer{
		WriteCloser: nopWriteCloser(io.Writer(b)),
		commitFunc: func() error {
			mc.c.Remove(key)
			_, done, _ := mc.c.Add(key, b)
			done(false)
			return nil
		},
		abortFunc: func() error {
			b.Reset()
			mc.bufPool.Put(b)
			return nil
		},
	}, nil
}

func (mc *ttlMemoryCache) Close() error {
	return nil
}
