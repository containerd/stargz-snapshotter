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

package remote

import (
	"bytes"
	"fmt"
	"sync"
	"syscall"
	"testing"

	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/cache"
	commonmetrics "github.com/containerd/stargz-snapshotter/fs/metrics/common"
	"github.com/prometheus/client_golang/prometheus"
)

type failAt int

const (
	failAdd failAt = iota
	failWrite
	failCommit
)

// noSpaceCache is a cache that never has room: depending on failAt, Add,
// Write or Commit returns ENOSPC. Nothing is ever stored, so Get always misses.
type noSpaceCache struct {
	failAt  failAt
	mu      sync.Mutex
	aborted int
}

func (c *noSpaceCache) Add(key string, opts ...cache.Option) (cache.Writer, error) {
	if c.failAt == failAdd {
		return nil, syscall.ENOSPC
	}
	return &noSpaceWriter{c: c}, nil
}

func (c *noSpaceCache) Get(key string, opts ...cache.Option) (cache.Reader, error) {
	return nil, syscall.ENOENT
}

func (c *noSpaceCache) Close() error { return nil }

type noSpaceWriter struct{ c *noSpaceCache }

func (w *noSpaceWriter) Write(p []byte) (int, error) {
	if w.c.failAt == failWrite {
		return 0, syscall.ENOSPC
	}
	return len(p), nil
}

func (w *noSpaceWriter) Commit() error {
	if w.c.failAt == failCommit {
		return syscall.ENOSPC
	}
	return nil
}

func (w *noSpaceWriter) Abort() error {
	w.c.mu.Lock()
	w.c.aborted++
	w.c.mu.Unlock()
	return nil
}

func (w *noSpaceWriter) Close() error { return nil }

// skippedCount returns the stargz_fs_cache_writes_skipped_total series for
// the unlabelled layer, or 0 if it does not exist.
func skippedCount(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "stargz_fs_cache_writes_skipped_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "layer" && l.GetValue() == "" {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestReadAtServesBytesWhenCacheIsFull checks that ENOSPC in the cache, at
// any step of adding a chunk, still serves the fetched bytes to the reader,
// including to readers that joined another reader's fetch.
func TestReadAtServesBytesWhenCacheIsFull(t *testing.T) {
	commonmetrics.Register(log.InfoLevel)

	for _, tc := range []struct {
		name   string
		failAt failAt
	}{
		{"Add", failAdd},
		{"Write", failWrite},
		{"Commit", failCommit},
	} {
		for _, readers := range []int{1, 4} {
			t.Run(fmt.Sprintf("%s/readers=%d", tc.name, readers), func(t *testing.T) {
				contents := []byte(sampleData1)
				nc := &noSpaceCache{failAt: tc.failAt}
				b := makeTestBlob(t, int64(len(contents)), sampleChunkSize, sampleChunkSize,
					multiRoundTripper(t, contents))
				b.cache = nc

				before := skippedCount(t)

				var wg sync.WaitGroup
				errs := make([]error, readers)
				got := make([][]byte, readers)
				for i := range readers {
					got[i] = make([]byte, len(contents))
					wg.Add(1)
					go func() {
						defer wg.Done()
						var n int
						n, errs[i] = b.ReadAt(got[i], 0)
						got[i] = got[i][:n]
					}()
				}
				wg.Wait()

				for i := range readers {
					if errs[i] != nil {
						t.Fatalf("reader %d: ReadAt failed although only the cache was full: %v", i, errs[i])
					}
					if !bytes.Equal(got[i], contents) {
						t.Fatalf("reader %d: ReadAt = %q, want %q", i, got[i], contents)
					}
				}

				chunks := (int64(len(contents)) + sampleChunkSize - 1) / sampleChunkSize
				if d := skippedCount(t) - before; d < float64(chunks) {
					t.Errorf("skipped-caching counter grew by %v, want at least %d", d, chunks)
				}
				if tc.failAt != failAdd && nc.aborted < int(chunks) {
					t.Errorf("staging writer aborted %d times, want at least %d", nc.aborted, chunks)
				}
			})
		}
	}
}
