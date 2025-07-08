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

package cacheutil

import (
	"sync"
	"testing"
	"time"
)

// TestTTLAdd tests Add API
func TestTTLAdd(t *testing.T) {
	c := NewTTLCache(time.Hour)

	key, value := "key1", "abcd"
	v, _, added := c.Add(key, value)
	if !added {
		t.Fatalf("failed to add %q", key)
	} else if v.(string) != value {
		t.Fatalf("returned different object for %q; want %q; got %q", key, value, v.(string))
	}

	key, newvalue := "key1", "dummy"
	v, _, added = c.Add(key, newvalue)
	if added || v.(string) != value {
		t.Fatalf("%q must be originally stored one; want %q; got %q (added:%v)",
			key, value, v.(string), added)
	}
}

// TestTTLGet tests Get API
func TestTTLGet(t *testing.T) {
	c := NewTTLCache(time.Hour)

	key, value := "key1", "abcd"
	v, _, added := c.Add(key, value)
	if !added {
		t.Fatalf("failed to add %q", key)
	} else if v.(string) != value {
		t.Fatalf("returned different object for %q; want %q; got %q", key, value, v.(string))
	}

	v, _, ok := c.Get(key)
	if !ok {
		t.Fatalf("failed to get obj %q (%q)", key, value)
	} else if v.(string) != value {
		t.Fatalf("unexpected object for %q; want %q; got %q", key, value, v.(string))
	}
}

// TestTTLRemove tests Remove API
func TestTTLRemove(t *testing.T) {
	var evicted []string
	c := NewTTLCache(time.Hour)
	c.OnEvicted = func(key string, value interface{}) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	_, done1, _ := c.Add(key1, value1)
	_, done12, _ := c.Get(key1)

	c.Remove(key1)
	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted after remove")
	}

	done1(false)
	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted until all reference are discarded")
	}

	done12(false)
	if len(evicted) != 1 {
		t.Fatalf("content must be evicted")
	}
	if evicted[0] != key1 {
		t.Fatalf("1st content %q must be evicted but got %q", key1, evicted[0])
	}
}

// TestTTLRemoveOverwritten tests old gc doesn't affect overwritten content
func TestTTLRemoveOverwritten(t *testing.T) {
	var evicted []string
	c := NewTTLCache(3 * time.Second)
	c.OnEvicted = func(key string, value interface{}) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	_, done1, _ := c.Add(key1, value1)
	done1(false)
	c.Remove(key1) // remove key1 as soon as possible

	// add another content with a new key
	time.Sleep(2 * time.Second)
	value12 := value1 + "!"
	_, done12, _ := c.Add(key1, value12)
	time.Sleep(2 * time.Second)
	// spent 4 sec (larger than ttl) since the previous key1 was added.
	// but the *newly-added* key1 hasn't been expierd yet so key1 must remain.
	v1, done122, getOK := c.Get(key1)
	if !getOK {
		t.Fatalf("unexpected eviction")
	}
	if s1, ok := v1.(string); !ok || s1 != value12 {
		t.Fatalf("unexpected content %q(%v) != %q", s1, ok, value12)
	}

	time.Sleep(2 * time.Second)
	done122(false)
	done12(false)
	// spent 4 sec since the new key1 was added. This should be expierd.
	if _, _, ok := c.Get(key1); ok {
		t.Fatalf("%q must be expierd but remaining", key1)
	}
}

// TestTTLEviction tests contents are evicted after TTL witout remaining reference.
func TestTTLEviction(t *testing.T) {
	var (
		evicted   []string
		evictedMu sync.Mutex
	)
	c := NewTTLCache(time.Second)
	c.OnEvicted = func(key string, value interface{}) {
		evictedMu.Lock()
		evicted = append(evicted, key)
		evictedMu.Unlock()
	}
	key1, value1 := "key1", "abcd1"
	key2, value2 := "key2", "abcd2"
	_, done1, _ := c.Add(key1, value1)
	done1(false) // evict key1 on expiering ttl
	_, done2, _ := c.Add(key2, value2)
	_, done22, _ := c.Get(key2) // hold reference of key2 to prevent eviction
	time.Sleep(3 * time.Second) // wait until elements reach ttl

	evictedMu.Lock()
	if len(evicted) != 1 {
		t.Fatalf("1 content must be removed")
	}
	if evicted[0] != key1 {
		t.Fatalf("1st content %q must be evicted but got %q", key1, evicted[0])
	}
	evictedMu.Unlock()

	done2(false) // effective
	done2(false) // ignored
	done2(false) // ignored
	evictedMu.Lock()
	if len(evicted) != 1 {
		t.Fatalf("only 1 content must be evicted")
	}
	evictedMu.Unlock()

	done22(false)
	evictedMu.Lock()
	if len(evicted) != 2 {
		t.Fatalf("2 contents must be evicted")
	}
	if evicted[1] != key2 {
		t.Fatalf("2nd content %q must be evicted but got %q", key2, evicted[1])
	}
	evictedMu.Unlock()
}

// TestTTLQuickDone tests the case where "done" with the explicit evict
// is called before TTL
func TestTTLQuickDone(t *testing.T) {
	var evicted []string
	c := NewTTLCache(time.Hour)
	c.OnEvicted = func(key string, value interface{}) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	_, done1, _ := c.Add(key1, value1)
	_, done12, _ := c.Get(key1)

	done1(false)
	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted until all reference are discarded")
	}

	done12(true)
	if len(evicted) != 1 {
		t.Fatalf("content must be evicted on an explicit evict before TTL")
	}
	if evicted[0] != key1 {
		t.Fatalf("1st content %q must be evicted but got %q", key1, evicted[0])
	}
}
