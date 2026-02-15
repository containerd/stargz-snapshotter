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
	"fmt"
	"testing"
)

// TestLRUAdd tests Add API
func TestLRUAdd(t *testing.T) {
	c := NewLRUCache(10)

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

// TestLRUGet tests Get API
func TestLRUGet(t *testing.T) {
	c := NewLRUCache(10)

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

// TestLRURemoe tests Remove API
func TestLRURemove(t *testing.T) {
	var evicted []string
	c := NewLRUCache(2)
	c.OnEvicted = func(key string, value any) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	_, done1, _ := c.Add(key1, value1)
	_, done12, _ := c.Get(key1)

	c.Remove(key1)
	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted after remove")
	}

	done1()
	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted until all reference are discarded")
	}

	done12()
	if len(evicted) != 1 {
		t.Fatalf("content must be evicted")
	}
	if evicted[0] != key1 {
		t.Fatalf("1st content %q must be evicted but got %q", key1, evicted[0])
	}
}

// TestLRUEviction tests that eviction occurs when the overflow happens.
func TestLRUEviction(t *testing.T) {
	var evicted []string
	c := NewLRUCache(2)
	c.OnEvicted = func(key string, value any) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	key2, value2 := "key2", "abcd2"
	_, done1, _ := c.Add(key1, value1)
	_, done2, _ := c.Add(key2, value2)
	_, done22, _ := c.Get(key2)

	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted after addition")
	}
	for i := range 2 {
		c.Add(fmt.Sprintf("key-add-%d", i), fmt.Sprintf("abcd-add-%d", i))
	}
	if len(evicted) != 0 {
		t.Fatalf("no content must be evicted after overflow")
	}

	done1()
	if len(evicted) != 1 {
		t.Fatalf("1 content must be evicted")
	}
	if evicted[0] != key1 {
		t.Fatalf("1st content %q must be evicted but got %q", key1, evicted[0])
	}

	done2() // effective
	done2() // ignored
	done2() // ignored
	if len(evicted) != 1 {
		t.Fatalf("only 1 content must be evicted")
	}

	done22()
	if len(evicted) != 2 {
		t.Fatalf("2 contents must be evicted")
	}
	if evicted[1] != key2 {
		t.Fatalf("2nd content %q must be evicted but got %q", key2, evicted[1])
	}
}
