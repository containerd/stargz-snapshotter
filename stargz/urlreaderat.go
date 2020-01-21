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

package stargz

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
	url                string
	t                  http.RoundTripper
	size               int64
	chunkSize          int64
	cache              cache.BlobCache
	fetchedRegionSet   regionSet
	fetchedRegionSetMu sync.Mutex
}

type regionSet struct {
	rs []region
}

// add attempts to merge r to rs.rs
func (rs *regionSet) add(r region) {
	for i := range rs.rs {
		f := &rs.rs[i]
		if r.b <= f.b && f.b <= r.e && r.e <= f.e {
			f.b = r.b
			return
		}
		if f.b <= r.b && r.e <= f.e {
			return
		}
		if f.b <= r.b && r.b <= f.e && f.e <= r.e {
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

// region is HTTP-range-request-compliant range.
// "b" is beginning byte of the range and "e" is the end.
// "e" is must be inclusive along with HTTP's range expression.
type region struct{ b, e int64 }

func (c region) size() int64 {
	return c.e - c.b + 1
}

type walkFunc func(reg region) error

// walkChunks walks chunks from begin to end in order in the specified region.
func (r *urlReaderAt) walkChunks(allRegion region, walkFn walkFunc) error {
	for b := allRegion.b; b <= allRegion.e && b < r.size; b += r.chunkSize {
		reg := region{b, b + r.chunkSize - 1}
		if reg.e >= r.size {
			reg.e = r.size - 1
		}
		if err := walkFn(reg); err != nil {
			return err
		}
	}
	return nil
}

// ReadAt reads remote chunks from specified offset for the buffer size.
// It tries to fetch as many chunks as possible from local cache.
func (r *urlReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 || offset > r.size {
		return 0, nil
	}

	// Fetch all data.
	allRegion := region{floor(offset, r.chunkSize), ceil(offset+int64(len(p))-1, r.chunkSize) - 1}
	allData := map[region][]byte{}
	remotes := r.appendFromCache(allData, allRegion)
	if err := r.appendFromRemote(allData, remotes); err != nil {
		return 0, err
	}
	r.fetchedRegionSetMu.Lock()
	for reg := range allData {
		r.fetchedRegionSet.add(reg)
	}
	r.fetchedRegionSetMu.Unlock()

	// Write all chunks to the result buffer.
	var regionData []byte
	if err := r.walkChunks(allRegion, func(reg region) error {
		data := allData[reg]
		if int64(len(data)) != reg.size() {
			return fmt.Errorf("fetched chunk(%d, %d) size is invalid", reg.b, reg.e)
		}
		regionData = append(regionData, data...)
		if remotes[reg] {
			r.cache.Add(r.genID(reg), data)
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("failed to gather chunks for region (%d, %d): %v",
			allRegion.b, allRegion.e, err)
	}
	if remain := r.size - offset; int64(len(p)) > remain {
		if remain < 0 {
			remain = 0
		}
		p = p[:remain]
	}
	ro := offset - allRegion.b // relative offset from the base of the fetched region
	copy(p, regionData[ro:ro+int64(len(p))])
	return len(p), nil
}

// appendFromRemote fetches all specified chunks from local cache.
func (r *urlReaderAt) appendFromCache(allData map[region][]byte, whole region) map[region]bool {
	remotes := map[region]bool{}
	_ = r.walkChunks(whole, func(reg region) error {
		data, err := r.cache.Fetch(r.genID(reg))
		if err != nil || int64(len(data)) != reg.size() {
			remotes[reg] = true // missed cache, needs to fetch remotely.
			return nil
		}
		allData[reg] = data
		return nil
	})
	return remotes
}

// appendFromRemote fetches all specified chunks from remote store.
func (r *urlReaderAt) appendFromRemote(allData map[region][]byte, requests map[region]bool) error {
	if len(requests) == 0 {
		return nil
	}

	// request specified ranges.
	req, err := http.NewRequest("GET", r.url, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	ranges := "bytes=0-0," // dummy range to make sure the response to be multipart
	for reg := range requests {
		ranges += fmt.Sprintf("%d-%d,", reg.b, reg.e)
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
		return fmt.Errorf("unexpected status code on %q: %v", r.url, res.Status)
	}

	// If We get whole blob in one part(= status 200), we chunk and return them.
	if res.StatusCode == http.StatusOK {
		data, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("Failed to read response body: %v", err)
		}
		gotSize := int64(len(data))
		requiredSize := int64(0)
		for reg := range requests {
			allData[reg] = data[reg.b : reg.e+1]
			requiredSize += reg.e - reg.b + 1
		}
		if requiredSize != gotSize {
			return fmt.Errorf("broken response body; want size %d but got %d", requiredSize, gotSize)
		}
		return nil
	}

	// Get all chunks responsed as a multipart body.
	mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return fmt.Errorf("invalid media type %q: %v", mediaType, err)
	}
	mr := multipart.NewReader(res.Body, params["boundary"])
	mr.NextPart() // Drop the dummy range.
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("Failed to read multipart response: %v", err)
		}
		reg, err := r.parseRange(p.Header.Get("Content-Range"))
		if err != nil {
			return fmt.Errorf("failed to parse Content-Range header: %v", err)
		}
		data, err := ioutil.ReadAll(p)
		if err != nil {
			return fmt.Errorf("failed to read multipart response data: %v", err)
		}
		if reg.size() != int64(len(data)) {
			return fmt.Errorf("chunk has invalid size %d; want %d", len(data), reg.size())
		}
		allData[reg] = data
	}

	return nil
}

func (r *urlReaderAt) parseRange(header string) (reg region, err error) {
	submatches := contentRangeRegexp.FindStringSubmatch(header)
	if len(submatches) < 4 {
		err = fmt.Errorf("Content-Range doesn't have enough information")
		return
	}
	begin, err := strconv.ParseInt(submatches[1], 10, 64)
	if err != nil {
		err = fmt.Errorf("failed to parse beginning offset: %v", err)
		return
	}
	end, err := strconv.ParseInt(submatches[2], 10, 64)
	if err != nil {
		err = fmt.Errorf("failed to parse end offset: %v", err)
		return
	}

	return region{begin, end}, nil
}

func (r *urlReaderAt) genID(reg region) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", r.url, reg.b, reg.e)))
	return fmt.Sprintf("%x", sum)
}

func floor(n int64, unit int64) int64 {
	return (n / unit) * unit
}

func ceil(n int64, unit int64) int64 {
	return (n/unit + 1) * unit
}

func (r *urlReaderAt) getFetchedSize() int64 {
	r.fetchedRegionSetMu.Lock()
	sz := r.fetchedRegionSet.totalSize()
	r.fetchedRegionSetMu.Unlock()
	return sz
}
