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
	"os"
	"path/filepath"
	"sync"
)

// TODO: contents validation.

type BlobCache interface {
	Fetch(blobHash string, p []byte) (int, error)
	Add(blobHash string, p []byte)
}

func NewDirectoryCache(directory string) (BlobCache, error) {
	err := os.MkdirAll(directory, os.ModePerm)
	if err != nil {
		return nil, err
	}
	return &directoryCache{
		directory: directory,
	}, nil
}

// directoryCache is a cache implementation which backend is a directory.
type directoryCache struct {
	directory string
	mu        sync.Mutex
}

func (dc *directoryCache) Fetch(blobHash string, p []byte) (n int, err error) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	c := filepath.Join(dc.directory, blobHash[:2], blobHash)
	if _, err := os.Stat(c); err != nil {
		return 0, fmt.Errorf("Missed cache: %s", c)
	}

	file, err := os.Open(c)
	if err != nil {
		return 0, fmt.Errorf("Failed to Open cached blob file: %s", c)
	}
	defer file.Close()

	if n, err = io.ReadFull(file, p); err != nil {
		if err == io.EOF {
			err = nil
		} else {
			return 0, err
		}
	}

	return n, err
}

func (dc *directoryCache) Add(blobHash string, p []byte) {
	dc.mu.Lock()
	defer dc.mu.Unlock()

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

	return
}

func NewMemoryCache() BlobCache {
	return &memoryCache{
		membuf: map[string]([]byte){},
	}
}

// meomryCache is a cache implementation which backend is a memory.
type memoryCache struct {
	membuf map[string]([]byte)
	mu     sync.Mutex
}

func (mc *memoryCache) Fetch(blobHash string, p []byte) (int, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	cache, ok := mc.membuf[blobHash]
	if !ok {
		return 0, fmt.Errorf("Missed cache: %s", blobHash)
	}
	if len(p) < len(cache) {
		return 0, fmt.Errorf("Buffer doesn't have enough length: %d(buffer) < %d(cache)",
			len(p), len(cache))
	}

	return copy(p[:len(cache)], cache[:len(cache)]), nil
}

func (mc *memoryCache) Add(blobHash string, p []byte) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.membuf[blobHash] = p

	return
}

// nopCache is a cache implementation which doesn't cache anything.
type nopCache struct{}

func NewNopCache() BlobCache {
	return &nopCache{}
}

func (nc *nopCache) Fetch(blobHash string, p []byte) (int, error) {
	return 0, fmt.Errorf("Missed cache: %s", blobHash)
}

func (nc *nopCache) Add(blobHash string, p []byte) {
	return
}
