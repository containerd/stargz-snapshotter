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

// region is HTTP-range-request-compliant range.
// "b" is beginning byte of the range and "e" is the end.
// "e" is must be inclusive along with HTTP's range expression.
type region struct{ b, e int64 }

func (c region) size() int64 {
	return c.e - c.b + 1
}

// regionSet is a set of regions
type regionSet struct {
	rs []region
}

// add attempts to merge r to rs.rs
func (rs *regionSet) add(r region) {
	for i := range rs.rs {
		f := &rs.rs[i]
		if r.b <= f.b && f.b <= r.e+1 && r.e <= f.e {
			f.b = r.b
			return
		}
		if f.b <= r.b && r.e <= f.e {
			return
		}
		if f.b <= r.b && r.b <= f.e+1 && f.e <= r.e {
			f.e = r.e
			return
		}
		if r.b <= f.b && f.e <= r.e {
			f.b = r.b
			f.e = r.e
			return
		}
	}
	rs.rs = append(rs.rs, r)
}

func (rs *regionSet) totalSize() int64 {
	var sz int64
	for _, f := range rs.rs {
		sz += f.size()
	}
	return sz
}
