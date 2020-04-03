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
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testURL            = "http://testdummy.com/v2/library/test/blobs/sha256:deadbeaf"
	rangeHeaderPrefix  = "bytes="
	sampleChunkSize    = 3
	sampleMiddleOffset = sampleChunkSize / 2
	sampleData1        = "0123456789"
	lastChunkOffset1   = sampleChunkSize * (int64(len(sampleData1)) / sampleChunkSize)
)

func TestCheckWithRoundTripper(t *testing.T) {
	tr := &breakRoundTripper{}
	ur := &Blob{
		url: "test",
		tr:  tr,
	}
	tr.success = true
	if err := ur.Check(); err != nil {
		t.Errorf("connection failed; wanted to succeed")
	}

	tr.success = false
	if err := ur.Check(); err == nil {
		t.Errorf("connection succeeded; wanted to fail")
	}
}

// Tests ReadAt method of each file.
func TestReadAt(t *testing.T) {
	sizeCond := map[string]int64{
		"single_chunk": sampleChunkSize - sampleMiddleOffset,
		"multi_chunks": 2*sampleChunkSize + sampleMiddleOffset,
	}
	innerOffsetCond := map[string]int64{
		"at_top":    0,
		"at_middle": sampleMiddleOffset,
	}
	baseOffsetCond := map[string]int64{
		"of_1st_chunk":  sampleChunkSize * 0,
		"of_2nd_chunk":  sampleChunkSize * 1,
		"of_last_chunk": sampleChunkSize * (int64(len(sampleData1)) / sampleChunkSize),
	}
	blobSizeCond := map[string]int64{
		"in_1_chunk_blob":  sampleChunkSize * 1,
		"in_3_chunks_blob": sampleChunkSize * 3,
		"in_max_size_blob": int64(len(sampleData1)),
	}
	cacheCond := map[string][]region{
		"with_clean_cache": nil,
		"with_edge_filled_cache": {
			region{0, sampleChunkSize - 1},
			region{lastChunkOffset1, int64(len(sampleData1)) - 1},
		},
		"with_sparse_cache": {
			region{0, sampleChunkSize - 1},
			region{2 * sampleChunkSize, 3*sampleChunkSize - 1},
		},
	}

	for sn, size := range sizeCond {
		for in, innero := range innerOffsetCond {
			for bo, baseo := range baseOffsetCond {
				for bs, blobsize := range blobSizeCond {
					for cc, cacheExcept := range cacheCond {
						t.Run(fmt.Sprintf("reading_%s_%s_%s_%s_%s", sn, in, bo, bs, cc), func(t *testing.T) {
							if blobsize > int64(len(sampleData1)) {
								t.Fatal("sample file size is larger than sample data")
							}

							wantN := size
							offset := baseo + innero
							if remain := blobsize - offset; remain < wantN {
								if wantN = remain; wantN < 0 {
									wantN = 0
								}
							}

							// use constant string value as a data source.
							want := strings.NewReader(sampleData1)

							// data we want to get.
							wantData := make([]byte, wantN)
							_, err := want.ReadAt(wantData, offset)
							if err != nil && err != io.EOF {
								t.Fatalf("want.ReadAt (offset=%d,size=%d): %v", offset, wantN, err)
							}

							// data we get through a remote blob.
							blob := []byte(sampleData1)[:blobsize]
							r := makeBlob(t, blob, sampleChunkSize, multiRoundTripper(t, blob, cacheExcept...))
							for _, reg := range cacheExcept {
								r.cache.Add(r.genID(reg), []byte(sampleData1[reg.b:reg.e+1]))
							}
							respData := make([]byte, size)
							n, err := r.ReadAt(respData, offset)
							if err != nil {
								t.Errorf("failed to read off=%d, size=%d, blobsize=%d: %v", offset, size, blobsize, err)
								return
							}
							respData = respData[:n]

							if !bytes.Equal(wantData, respData) {
								t.Errorf("off=%d, blobsize=%d; read data{size=%d,data=%q}; want (size=%d,data=%q)",
									offset, blobsize, len(respData), string(respData), wantN, string(wantData))
								return
							}

							// check cache has valid contents.
							cn := 0
							whole := region{floor(offset, r.chunkSize), ceil(offset+size-1, r.chunkSize) - 1}
							if err := r.walkChunks(whole, func(reg region) error {
								data, err := r.cache.Fetch(r.genID(reg))
								if err != nil || int64(len(data)) != reg.size() {
									return fmt.Errorf("missed cache of region={%d,%d}(size=%d): %v(got size=%d)", reg.b, reg.e, reg.size(), err, n)
								}
								cn++
								return nil
							}); err != nil {
								t.Errorf("%v", err)
								return
							}
						})
					}
				}
			}
		}
	}
}

// Tests ReadAt method for failure cases.
func TestFailReadAt(t *testing.T) {

	// test failed http respose.
	r := makeBlob(t, []byte(sampleData1), sampleChunkSize, failRoundTripper())
	respData := make([]byte, len(sampleData1))
	_, err := r.ReadAt(respData, 0)
	if err == nil || err == io.EOF {
		t.Errorf("must be fail for http failure but err=%v", err)
		return
	}

	// test broken body
	r = makeBlob(t, []byte(sampleData1), sampleChunkSize, brokenBodyRoundTripper(t, []byte(sampleData1)))
	respData = make([]byte, len(sampleData1))
	_, err = r.ReadAt(respData, 0)
	if err == nil || err == io.EOF {
		t.Errorf("must be fail for broken full body but err=%v", err)
		return
	}
	_, err = r.ReadAt(respData[0:len(sampleData1)/2], 0)
	if err == nil || err == io.EOF {
		t.Errorf("must be fail for broken multipart body but err=%v", err)
		return
	}

	// test broken header
	r = makeBlob(t, []byte(sampleData1), sampleChunkSize, brokenHeaderRoundTripper(t, []byte(sampleData1)))
	respData = make([]byte, len(sampleData1))
	_, err = r.ReadAt(respData[0:len(sampleData1)/2], 0)
	if err == nil || err == io.EOF {
		t.Errorf("must be fail for broken multipart header but err=%v", err)
		return
	}
}

func makeBlob(t *testing.T, contents []byte, chunkSize int64, fn RoundTripFunc) *Blob {
	return &Blob{
		url:       testURL,
		tr:        fn,
		size:      int64(len(contents)),
		chunkSize: chunkSize,
		cache:     &testCache{membuf: map[string]string{}, t: t},
	}
}

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
			expected: []region{{1, 5}, {6, 8}},
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

type testCache struct {
	membuf map[string]string
	t      *testing.T
	mu     sync.Mutex
}

func (tc *testCache) Fetch(blobHash string) ([]byte, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	cache, ok := tc.membuf[blobHash]
	if !ok {
		return nil, fmt.Errorf("Missed cache: %q", blobHash)
	}
	return []byte(cache), nil
}

func (tc *testCache) Add(blobHash string, p []byte) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.membuf[blobHash] = string(p)
	tc.t.Logf("  cached [%s...]: %q", blobHash[:8], string(p))
}

func TestCheckInterval(t *testing.T) {
	var (
		largeInterval = time.Hour
		tr            = &calledRoundTripper{}
		firstTime     = time.Now()
		ur            = &Blob{
			tr:            tr,
			lastCheck:     firstTime,
			checkInterval: largeInterval,
		}
	)

	check := func(name string) (time.Time, bool) {
		beforeUpdate := time.Now()

		time.Sleep(time.Millisecond)

		tr.called = false
		if err := ur.Check(); err != nil {
			t.Fatalf("%q: check mustn't be failed", name)
		}

		time.Sleep(time.Millisecond)

		afterUpdate := time.Now()
		if !tr.called {
			return ur.lastCheck, false
		}
		if !(ur.lastCheck.After(beforeUpdate) && ur.lastCheck.Before(afterUpdate)) {
			t.Errorf("%q: updated time must be after %q and before %q but %q", name, beforeUpdate, afterUpdate, ur.lastCheck)
		}

		return ur.lastCheck, true
	}

	// second time(not expired yet)
	secondTime, called := check("second time")
	if called {
		t.Error("mustn't be checked if not expired")
	}
	if !secondTime.Equal(firstTime) {
		t.Errorf("lastCheck time must be same as first time(%q) but %q", firstTime, secondTime)
	}

	// third time(expired, must be checked)
	ur.lastCheck = time.Now().Add(-1 * largeInterval) // set to "largeInterval" ago
	if _, called := check("third time"); !called {
		t.Error("must be called for the third time")
	}
}

type breakRoundTripper struct {
	success bool
}

func (b *breakRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	if b.success {
		res = &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     make(http.Header),
			Body:       ioutil.NopCloser(bytes.NewReader([]byte("test"))),
		}
	} else {
		res = &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
		}
	}
	return
}

type calledRoundTripper struct {
	called bool
}

func (c *calledRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	c.called = true
	res = &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("test"))),
	}
	return
}

type RoundTripFunc func(req *http.Request) *http.Response

func (f RoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func multiRoundTripper(t *testing.T, contents []byte, except ...region) RoundTripFunc {
	ce := map[region]bool{}
	for _, reg := range except {
		ce[reg] = true
	}
	return func(req *http.Request) *http.Response {
		// Validate request
		if req.Method != "GET" || req.URL.String() != testURL {
			return &http.Response{StatusCode: http.StatusNotFound}
		}
		ranges := req.Header.Get("Range")
		if ranges == "" {
			return &http.Response{StatusCode: http.StatusNotFound}
		}
		if !strings.HasPrefix(ranges, rangeHeaderPrefix) {
			return &http.Response{StatusCode: http.StatusNotFound}
		}

		// check this request can be served as one whole blob.
		var sorted []region
		for _, part := range strings.Split(ranges[len(rangeHeaderPrefix):], ",") {
			begin, end := parseRange(t, part)
			sorted = append(sorted, region{begin, end})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].b < sorted[j].b
		})
		var sparse bool
		if sorted[0].b == 0 {
			var max int64
			for _, reg := range sorted {
				if reg.e > max {
					if max < reg.b-1 {
						sparse = true
						break
					}
					max = reg.e
				}
			}
			if max >= int64(len(contents)-1) && !sparse {
				t.Logf("serving whole range %q = %d", ranges, len(contents))
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       ioutil.NopCloser(bytes.NewReader(contents)),
				}
			}
		}

		// Write multipart response.
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		parts := strings.Split(ranges[len(rangeHeaderPrefix):], ",")
		for _, part := range parts {
			mh := make(textproto.MIMEHeader)
			mh.Set("Content-Range", fmt.Sprintf("bytes %s/%d", part, len(contents)))
			w, err := mw.CreatePart(mh)
			if err != nil {
				t.Fatalf("failed to create part: %v", err)
			}
			begin, end := parseRange(t, part)
			if begin >= int64(len(contents)) {
				// skip if out of range.
				continue
			}
			if end > int64(len(contents)-1) {
				end = int64(len(contents) - 1)
			}
			if ce[region{begin, end}] {
				t.Fatalf("Requested prohibited region of chunk: (%d, %d)", begin, end)
			}
			if n, err := w.Write(contents[begin : end+1]); err != nil || int64(n) != end+1-begin {
				t.Fatalf("failed to write to part(%d-%d): %v", begin, end, err)
			}
		}
		mw.Close()
		param := map[string]string{
			"boundary": mw.Boundary(),
		}
		header := make(http.Header)
		header.Add("Content-Type", mime.FormatMediaType("multipart/text", param))
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     header,
			Body:       ioutil.NopCloser(&buf),
		}
	}
}

func failRoundTripper() RoundTripFunc {
	return func(req *http.Request) *http.Response {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
		}
	}
}

func brokenBodyRoundTripper(t *testing.T, contents []byte) RoundTripFunc {
	return func(req *http.Request) *http.Response {
		// Validate request
		if req.Method != "GET" || req.URL.String() != testURL {
			return &http.Response{StatusCode: http.StatusNotFound}
		}
		ranges := req.Header.Get("Range")
		if ranges == "" {
			return &http.Response{StatusCode: http.StatusNotFound}
		}
		if !strings.HasPrefix(ranges, rangeHeaderPrefix) {
			return &http.Response{StatusCode: http.StatusNotFound}
		}

		// check this request can be served as one whole blob.
		var sorted []region
		for _, part := range strings.Split(ranges[len(rangeHeaderPrefix):], ",") {
			begin, end := parseRange(t, part)
			sorted = append(sorted, region{begin, end})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].b < sorted[j].b
		})
		var sparse bool
		if sorted[0].b == 0 {
			var max int64
			for _, reg := range sorted {
				if reg.e > max {
					if max < reg.b-1 {
						sparse = true
						break
					}
					max = reg.e
				}
			}
			if max >= int64(len(contents)-1) && !sparse {
				t.Logf("serving whole range %q = %d [but broken range!]", ranges, len(contents))
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       ioutil.NopCloser(bytes.NewReader(contents[:len(contents)/2])),
				}
			}
		}

		// Write multipart response.
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		parts := strings.Split(ranges[len(rangeHeaderPrefix):], ",")
		for _, part := range parts {
			mh := make(textproto.MIMEHeader)
			mh.Set("Content-Range", fmt.Sprintf("bytes %s/%d", part, len(contents)))
			w, err := mw.CreatePart(mh)
			if err != nil {
				t.Fatalf("failed to create part: %v", err)
			}
			begin, end := parseRange(t, part)
			if begin >= int64(len(contents)) {
				// skip if out of range.
				continue
			}
			if end > int64(len(contents)-1) {
				end = int64(len(contents) - 1)
			}
			brokenEnd := (end + 1) / 2
			if n, err := w.Write(contents[begin:brokenEnd]); err != nil || int64(n) != brokenEnd-begin {
				t.Fatalf("failed to write to part(%d-%d): %v", begin, brokenEnd, err)
			}
		}
		mw.Close()
		param := map[string]string{
			"boundary": mw.Boundary(),
		}
		header := make(http.Header)
		header.Add("Content-Type", mime.FormatMediaType("multipart/text", param))
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     header,
			Body:       ioutil.NopCloser(&buf),
		}
	}
}

func brokenHeaderRoundTripper(t *testing.T, contents []byte) RoundTripFunc {
	tr := multiRoundTripper(t, contents)
	return func(req *http.Request) *http.Response {
		res := tr(req)
		res.Header = make(http.Header)
		return res
	}
}

func parseRange(t *testing.T, rangeString string) (int64, int64) {
	rng := strings.Split(rangeString, "-")
	if len(rng) != 2 {
		t.Fatalf("falied to parse range %q", rng)
	}
	begin, err := strconv.ParseInt(rng[0], 10, 64)
	if err != nil {
		t.Fatalf("failed to parse beginning offset: %v", err)
	}
	end, err := strconv.ParseInt(rng[1], 10, 64)
	if err != nil {
		t.Fatalf("failed to parse ending offset: %v", err)
	}
	return begin, end
}
