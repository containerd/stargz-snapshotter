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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package remote

import (
	"reflect"
	"testing"
)

func TestRegionSet(t *testing.T) {
	tests := []struct {
		input    []region
		expected []region
	}{
		{
			input:    []region{{1, 3}, {2, 4}},
			expected: []region{{1, 4}},
		},
		{
			input:    []region{{1, 5}, {2, 4}},
			expected: []region{{1, 5}},
		},
		{
			input:    []region{{2, 4}, {1, 5}},
			expected: []region{{1, 5}},
		},
		{
			input:    []region{{2, 4}, {6, 8}, {1, 5}},
			expected: []region{{1, 8}},
		},
		{
			input:    []region{{1, 2}, {1, 2}},
			expected: []region{{1, 2}},
		},
		{
			input:    []region{{1, 3}, {1, 2}},
			expected: []region{{1, 3}},
		},
		{
			input:    []region{{1, 3}, {2, 3}},
			expected: []region{{1, 3}},
		},
		{
			input:    []region{{1, 3}, {3, 6}},
			expected: []region{{1, 6}},
		},
		{
			input:    []region{{1, 3}, {4, 6}}, // region.e is inclusive
			expected: []region{{1, 6}},
		},
		{
			input:    []region{{4, 6}, {1, 3}}, // region.e is inclusive
			expected: []region{{1, 6}},
		},
		{
			input:    []region{{4, 6}, {1, 3}, {7, 9}, {2, 8}},
			expected: []region{{1, 9}},
		},
		{
			input:    []region{{4, 6}, {1, 5}, {7, 9}, {4, 8}},
			expected: []region{{1, 9}},
		},
		{
			input:    []region{{7, 8}, {1, 2}, {5, 6}},
			expected: []region{{1, 2}, {5, 8}},
		},
	}
	for i, tt := range tests {
		var rs regionSet
		for _, f := range tt.input {
			rs.add(f)
		}
		if !reflect.DeepEqual(tt.expected, rs.rs) {
			t.Errorf("#%d: expected %v, got %v", i, tt.expected, rs.rs)
		}
	}
}
