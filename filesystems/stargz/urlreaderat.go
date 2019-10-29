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

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ktock/remote-snapshotter/cache"
)

var contentRangeRegexp = regexp.MustCompile(`bytes ([0-9]+)-([0-9]+)/([0-9]+|\\*)`)

type urlReaderAt struct {
	url       string
	t         http.RoundTripper
	size      int64
	chunkSize int64
	cache     cache.BlobCache
}

type chunk struct {
	size   int64
	buffer []byte
}

func (r *urlReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	// Align offset and size with chunkSize (call it "region") and make chunks list to
	// get from the remote store. At the same time, fetch as many chunks of them as
	// possible from the cache.
	regionOffset := floor(offset, r.chunkSize)
	regionSize := ceil(offset+int64(len(p))-1, r.chunkSize) - regionOffset
	region := make([]byte, regionSize)
	chunks := map[int64](*chunk){}
	for o := regionOffset; o < regionOffset+regionSize; o += r.chunkSize {
		size := r.chunkSize
		if remain := r.size - o; remain < size {
			size = remain
		}
		ro := o - regionOffset
		n, err := r.cache.Fetch(r.genID(o, size), region[ro:ro+size])
		if err == nil && int64(n) == size {
			continue
		}
		chunks[o] = &chunk{
			size: size,
		}
	}

	// Get all chunks from the remote store.
	if err := r.rangeRequest(chunks); err != nil {
		return 0, err
	}

	// Write all chunks to the result buffer.
	wg := &sync.WaitGroup{}
	for o, c := range chunks {
		wg.Add(1)
		go func(o int64, c *chunk) {
			ro := o - regionOffset
			copy(region[ro:ro+c.size], c.buffer)
			r.cache.Add(r.genID(o, c.size), region[ro:ro+c.size])
			wg.Done()
		}(o, c)
	}
	wg.Wait()

	ro := offset % r.chunkSize
	return copy(p, region[ro:ro+int64(len(p))]), nil
}

func (r *urlReaderAt) rangeRequest(chunks map[int64](*chunk)) error {
	if len(chunks) == 0 {
		return nil
	}
	req, err := http.NewRequest("GET", r.url, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	// Construct request.
	ranges := "bytes=0-0," // dummy range to make sure the response to be multipart
	for o, c := range chunks {
		ranges += fmt.Sprintf("%d-%d,", o, o+c.size-1)
	}
	req.Header.Add("Range", ranges[:len(ranges)-1])
	req.Header.Add("Accept-Encoding", "identity")
	req.Close = false
	res, err := r.t.RoundTrip(req) // NOT DefaultClient; don't want redirects
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code on %s: %v", r.url, res.Status)
	}
	if res.StatusCode == http.StatusOK {
		// We reach here when the requested range covers whole blob. We got whole
		// blob in one part so we need to chunk it.
		data, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("Failed to read response body: %v", err)
		}
		for o, c := range chunks {
			if remain := int64(len(data)) - o; c.size > remain {
				c.size = remain
			}
			c.buffer = data[o : o+c.size]
		}
		return nil
	}

	// Extract multipart parameters.
	mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("failed to parse media type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return fmt.Errorf("the response is not mulipart: %q", mediaType)
	}

	// Parse parts and fill returning chunks.
	mr := multipart.NewReader(res.Body, params["boundary"])
	mr.NextPart() // Drop the dummy range.
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("Failed to read multipart response: %v\n", err)
		}

		// Check Content-Range Header.
		submatches := contentRangeRegexp.FindStringSubmatch(p.Header.Get("Content-Range"))
		if len(submatches) < 4 {
			return fmt.Errorf("Content-Range doesn't have enough information")
		}
		offset, err := strconv.ParseInt(submatches[1], 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse beginning offset: %v", err)
		}
		c, ok := chunks[offset]
		if !ok {
			// This chunk isn't requred.
			continue
		}

		// Read this chunk.
		data, err := ioutil.ReadAll(p)
		if err != nil {
			return fmt.Errorf("failed to read multipart response data: %v", err)
		}
		if remain := r.size - offset; c.size > remain {
			c.size = remain
		}
		if c.size != int64(len(data)) {
			return fmt.Errorf("fetched data size is invalid: %d(expected) != %d(fetched)",
				c.size, len(data))
		}
		c.buffer = data[:c.size]
	}

	// Make sure all requested chunks have been fetched.
	for o, c := range chunks {
		if c.buffer == nil {
			return fmt.Errorf("Failed to fetch chunk offset:%s,size:%s", o, c.size)
		}
	}

	return nil
}

func (r *urlReaderAt) genID(offset, size int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", r.url, offset, size)))
	return fmt.Sprintf("%x", sum)
}

func floor(n int64, unit int64) int64 {
	return (n / unit) * unit
}

func ceil(n int64, unit int64) int64 {
	return (n/unit + 1) * unit
}
