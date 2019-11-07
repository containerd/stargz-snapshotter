// +build linux

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
)

// TODO: contents validation.

type BlobCache interface {
	Fetch(blobHash string) ([]byte, error)
	Add(blobHash string, p []byte)
}

func NewDirectoryCache(directory string, memCacheSize int) (BlobCache, error) {
	err := os.MkdirAll(directory, os.ModePerm)
	if err != nil {
		return nil, err
	}
	return &directoryCache{
		cache:     lru.New(memCacheSize),
		directory: directory,
	}, nil
}

// directoryCache is a cache implementation which backend is a directory.
type directoryCache struct {
	cache     *lru.Cache
	directory string
	mmu       sync.Mutex
	fmu       sync.Mutex
}

func (dc *directoryCache) Fetch(blobHash string) (p []byte, err error) {
	dc.mmu.Lock()
	defer dc.mmu.Unlock()

	if cache, ok := dc.cache.Get(blobHash); ok {
		p, ok := cache.([]byte)
		if ok {
			return p, nil
		}
	}

	c := filepath.Join(dc.directory, blobHash[:2], blobHash)
	if _, err := os.Stat(c); err != nil {
		return nil, fmt.Errorf("Missed cache: %s", c)
	}

	file, err := os.Open(c)
	if err != nil {
		return nil, fmt.Errorf("Failed to Open cached blob file: %s", c)
	}
	defer file.Close()

	if p, err = ioutil.ReadAll(file); err != nil {
		if err != io.EOF {
			return nil, err
		}
	}
	dc.cache.Add(blobHash, p)

	return
}

func (dc *directoryCache) Add(blobHash string, p []byte) {
	dc.mmu.Lock()
	defer dc.mmu.Unlock()

	dc.cache.Add(blobHash, p)

	go func() {
		dc.fmu.Lock()
		defer dc.fmu.Unlock()

		// Check if cache exists.
		c := filepath.Join(dc.directory, blobHash[:2], blobHash)
		if _, err := os.Stat(c); err == nil {
			return
		}

		// Create cache file
		if err := os.MkdirAll(filepath.Dir(c), os.ModePerm); err != nil {
			fmt.Printf("Warning: Failed to Create blob cache directory %s: %s\n", c, err)
			return
		}
		f, err := os.Create(c)
		if err != nil {
			fmt.Printf("Warning: could not create a cache file: %v\n", err)
			return
		}
		defer f.Close()
		if n, err := f.Write(p); err == nil && n == len(p) {
			return
		} else {
			fmt.Printf("Warning: failed to write cache: %d(wrote)/%d(expected): %v\n",
				n, len(p), err)
		}
	}()

	return
}

func NewMemoryCache() BlobCache {
	return &memoryCache{
		membuf: map[string]string{},
	}
}

// meomryCache is a cache implementation which backend is a memory.
type memoryCache struct {
	membuf map[string]string // read-only []byte map is more ideal but we don't have it in golang...
	mu     sync.Mutex
}

func (mc *memoryCache) Fetch(blobHash string) ([]byte, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	cache, ok := mc.membuf[blobHash]
	if !ok {
		return nil, fmt.Errorf("Missed cache: %s", blobHash)
	}
	return []byte(cache), nil
}

func (mc *memoryCache) Add(blobHash string, p []byte) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.membuf[blobHash] = string(p)

	return
}

// nopCache is a cache implementation which doesn't cache anything.
type nopCache struct{}

func NewNopCache() BlobCache {
	return &nopCache{}
}

func (nc *nopCache) Fetch(blobHash string) ([]byte, error) {
	return nil, fmt.Errorf("Missed cache: %s", blobHash)
}

func (nc *nopCache) Add(blobHash string, p []byte) {
	return
}
