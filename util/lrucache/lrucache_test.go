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

package lrucache

import (
	"fmt"
	"testing"
)

func TestAdd(t *testing.T) {
	c := New(10)

	key, value := "key1", "abcd"
	v, _, added := c.Add(key, value)
	if !added {
		t.Errorf("failed to add %q", key)
		return
	} else if v.(string) != value {
		t.Errorf("returned different object for %q; want %q; got %q", key, value, v.(string))
		return
	}

	key, newvalue := "key1", "dummy"
	v, _, added = c.Add(key, newvalue)
	if added || v.(string) != value {
		t.Errorf("%q must be originally stored one; want %q; got %q (added:%v)",
			key, value, v.(string), added)
	}
}

func TestGet(t *testing.T) {
	c := New(10)

	key, value := "key1", "abcd"
	v, _, added := c.Add(key, value)
	if !added {
		t.Errorf("failed to add %q", key)
		return
	} else if v.(string) != value {
		t.Errorf("returned different object for %q; want %q; got %q", key, value, v.(string))
		return
	}

	v, _, ok := c.Get(key)
	if !ok {
		t.Errorf("failed to get obj %q (%q)", key, value)
		return
	} else if v.(string) != value {
		t.Errorf("unexpected object for %q; want %q; got %q", key, value, v.(string))
		return
	}
}

func TestRemove(t *testing.T) {
	var evicted []string
	c := New(2)
	c.OnEvicted = func(key string, value interface{}) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	_, done1, _ := c.Add(key1, value1)
	_, done12, _ := c.Get(key1)

	c.Remove(key1)
	if len(evicted) != 0 {
		t.Errorf("no content must be evicted after remove")
		return
	}

	done1()
	if len(evicted) != 0 {
		t.Errorf("no content must be evicted until all reference are discarded")
		return
	}

	done12()
	if len(evicted) != 1 {
		t.Errorf("content must be evicted")
		return
	}
	if evicted[0] != key1 {
		t.Errorf("1st content %q must be evicted but got %q", key1, evicted[0])
		return
	}
}

func TestEviction(t *testing.T) {
	var evicted []string
	c := New(2)
	c.OnEvicted = func(key string, value interface{}) {
		evicted = append(evicted, key)
	}
	key1, value1 := "key1", "abcd1"
	key2, value2 := "key2", "abcd2"
	_, done1, _ := c.Add(key1, value1)
	_, done2, _ := c.Add(key2, value2)
	_, done22, _ := c.Get(key2)

	if len(evicted) != 0 {
		t.Errorf("no content must be evicted after addition")
		return
	}
	for i := 0; i < 2; i++ {
		c.Add(fmt.Sprintf("key-add-%d", i), fmt.Sprintf("abcd-add-%d", i))
	}
	if len(evicted) != 0 {
		t.Errorf("no content must be evicted after overflow")
		return
	}

	done1()
	if len(evicted) != 1 {
		t.Errorf("1 content must be evicted")
		return
	}
	if evicted[0] != key1 {
		t.Errorf("1st content %q must be evicted but got %q", key1, evicted[0])
		return
	}

	done2() // effective
	done2() // ignored
	done2() // ignored
	if len(evicted) != 1 {
		t.Errorf("only 1 content must be evicted")
		return
	}

	done22()
	if len(evicted) != 2 {
		t.Errorf("2 contents must be evicted")
		return
	}
	if evicted[1] != key2 {
		t.Errorf("2nd content %q must be evicted but got %q", key2, evicted[1])
		return
	}
}
