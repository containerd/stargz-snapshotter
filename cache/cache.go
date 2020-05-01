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
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/golang/groupcache/lru"
	"github.com/pkg/errors"
)

// TODO: contents validation.

type BlobCache interface {
	Fetch(blobHash string) ([]byte, error)
	Add(blobHash string, p []byte)
}

type dirOpt struct {
	syncAdd bool
}

type DirOption func(o *dirOpt) *dirOpt

func SyncAdd() DirOption {
	return func(o *dirOpt) *dirOpt {
		o.syncAdd = true
		return o
	}
}

func NewDirectoryCache(directory string, memCacheSize int, opts ...DirOption) (BlobCache, error) {
	opt := &dirOpt{}
	for _, o := range opts {
		opt = o(opt)
	}
	if err := os.MkdirAll(directory, os.ModePerm); err != nil {
		return nil, err
	}
	dc := &directoryCache{
		cache:     lru.New(memCacheSize),
		directory: directory,
	}
	if opt.syncAdd {
		dc.syncAdd = true
	}
	return dc, nil
}

// directoryCache is a cache implementation which backend is a directory.
type directoryCache struct {
	cache     *lru.Cache
	cacheMu   sync.Mutex
	directory string
	syncAdd   bool
	fileMu    sync.Mutex
}

func (dc *directoryCache) Fetch(blobHash string) (p []byte, err error) {
	dc.cacheMu.Lock()
	defer dc.cacheMu.Unlock()

	if cache, ok := dc.cache.Get(blobHash); ok {
		p, ok := cache.([]byte)
		if ok {
			return p, nil
		}
	}

	c := filepath.Join(dc.directory, blobHash[:2], blobHash)
	if _, err := os.Stat(c); err != nil {
		return nil, errors.Wrapf(err, "Missed cache %q", c)
	}

	file, err := os.Open(c)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to Open cached blob file %q", c)
	}
	defer file.Close()

	if p, err = ioutil.ReadAll(file); err != nil && err != io.EOF {
		return nil, errors.Wrapf(err, "failed to read cached data %q", c)
	}
	dc.cache.Add(blobHash, p)

	return
}

func (dc *directoryCache) Add(blobHash string, p []byte) {
	// Copy the original data for avoiding the cached contents to be edited accidentally
	p2 := make([]byte, len(p))
	copy(p2, p)
	p = p2

	dc.cacheMu.Lock()
	dc.cache.Add(blobHash, p)
	dc.cacheMu.Unlock()

	addFunc := func() {
		dc.fileMu.Lock()
		defer dc.fileMu.Unlock()

		// Check if cache exists.
		c := filepath.Join(dc.directory, blobHash[:2], blobHash)
		if _, err := os.Stat(c); err == nil {
			return
		}

		// Create cache file
		if err := os.MkdirAll(filepath.Dir(c), os.ModePerm); err != nil {
			fmt.Printf("Warning: Failed to Create blob cache directory %q: %v\n", c, err)
			return
		}
		f, err := os.Create(c)
		if err != nil {
			fmt.Printf("Warning: could not create a cache file at %q: %v\n", c, err)
			return
		}
		defer f.Close()
		if n, err := f.Write(p); err != nil || n != len(p) {
			fmt.Printf("Warning: failed to write cache: %d(wrote)/%d(expected): %v\n",
				n, len(p), err)
		}
	}

	if dc.syncAdd {
		addFunc()
	} else {
		go addFunc()
	}
}

func NewMemoryCache() BlobCache {
	return &memoryCache{
		membuf: map[string]string{},
	}
}

// memoryCache is a cache implementation which backend is a memory.
type memoryCache struct {
	membuf map[string]string // read-only []byte map is more ideal but we don't have it in golang...
	mu     sync.Mutex
}

func (mc *memoryCache) Fetch(blobHash string) ([]byte, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	cache, ok := mc.membuf[blobHash]
	if !ok {
		return nil, fmt.Errorf("Missed cache: %q", blobHash)
	}
	return []byte(cache), nil
}

func (mc *memoryCache) Add(blobHash string, p []byte) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.membuf[blobHash] = string(p)
}
