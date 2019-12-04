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
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
)

const (
	sampleData = "0123456789"
)

func TestDirectoryCache(t *testing.T) {

	// with enough memory cache
	newCache := func() (BlobCache, cleanFunc) {
		tmp, err := ioutil.TempDir("", "testcache")
		if err != nil {
			t.Fatalf("failed to make tempdir: %v", err)
		}
		c, err := NewDirectoryCache(tmp, 10, SyncAdd())
		if err != nil {
			t.Fatalf("failed to make cache: %v", err)
		}
		return c, func() { os.RemoveAll(tmp) }
	}
	testCache(t, "dir-with-enough-mem", newCache)

	// with smaller memory cache
	newCache = func() (BlobCache, cleanFunc) {
		tmp, err := ioutil.TempDir("", "testcache")
		if err != nil {
			t.Fatalf("failed to make tempdir: %v", err)
		}
		c, err := NewDirectoryCache(tmp, 1, SyncAdd())
		if err != nil {
			t.Fatalf("failed to make cache: %v", err)
		}
		return c, func() { os.RemoveAll(tmp) }
	}
	testCache(t, "dir-with-small-mem", newCache)
}

func TestMemoryCache(t *testing.T) {
	testCache(t, "memory", func() (BlobCache, cleanFunc) { return NewMemoryCache(), func() {} })
}

type cleanFunc func()

func testCache(t *testing.T, name string, newCache func() (BlobCache, cleanFunc)) {
	tests := []struct {
		name   string
		blobs  []string
		checks []check
	}{
		{
			name: "empty_data",
			blobs: []string{
				"",
			},
			checks: []check{
				hit(""),
				miss(sampleData),
			},
		},
		{
			name: "data",
			blobs: []string{
				sampleData,
			},
			checks: []check{
				hit(sampleData),
				miss("dummy"),
			},
		},
		{
			name: "manydata",
			blobs: []string{
				sampleData,
				"test",
			},
			checks: []check{
				hit(sampleData),
				miss("dummy"),
			},
		},
		{
			name: "dup_data",
			blobs: []string{
				sampleData,
				sampleData,
			},
			checks: []check{
				hit(sampleData),
			},
		},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s-%s", name, tt.name), func(t *testing.T) {
			c, clean := newCache()
			defer clean()
			for _, blob := range tt.blobs {
				d := digestFor(blob)
				c.Add(d, []byte(blob))
			}
			for _, check := range tt.checks {
				check(t, c)
			}
		})
	}
}

type check func(*testing.T, BlobCache)

func digestFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

func hit(sample string) check {
	return func(t *testing.T, c BlobCache) {
		d := digestFor(sample)
		p, err := c.Fetch(d)
		if err != nil {
			t.Errorf("failed to fetch blob %q: %v", d, err)
			return
		}
		if len(p) != len(sample) {
			t.Errorf("fetched size %d; want %d", len(p), len(sample))
			return
		}
		df := digestFor(string(p))
		if df != d {
			t.Errorf("fetched digest %q(%q); want %q(%q)",
				df, string(p), d, sample)
		}
	}
}

func miss(sample string) check {
	return func(t *testing.T, c BlobCache) {
		d := digestFor(sample)
		_, err := c.Fetch(d)
		if err == nil {
			t.Errorf("hit blob %q but must be missed: %v", d, err)
			return
		}
	}
}
