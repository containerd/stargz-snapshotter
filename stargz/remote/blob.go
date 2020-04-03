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

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/pkg/errors"
)

var contentRangeRegexp = regexp.MustCompile(`bytes ([0-9]+)-([0-9]+)/([0-9]+|\\*)`)

type Blob struct {
	ref           name.Reference
	keychain      authn.Keychain
	url           string
	tr            http.RoundTripper
	size          int64
	chunkSize     int64
	cache         cache.BlobCache
	lastCheck     time.Time
	checkInterval time.Duration

	fetchedRegionSet   regionSet
	fetchedRegionSetMu sync.Mutex
}

func (r *Blob) Check() error {
	now := time.Now()
	if now.Sub(r.lastCheck) < r.checkInterval {
		// do nothing if not expired
		return nil
	}
	r.lastCheck = now

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequest("GET", r.url, nil)
	if err != nil {
		return errors.Wrapf(err, "check failed: failed to make request for %q", r.url)
	}
	req = req.WithContext(ctx)
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := r.tr.RoundTrip(req)
	if err != nil {
		return errors.Wrapf(err, "check failed: failed to request to registry %q", r.url)
	}
	defer func() {
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code %v for %q", res.StatusCode, r.url)
	}

	return nil
}

func (r *Blob) Authn(tr http.RoundTripper) (http.RoundTripper, error) {
	return authnTransport(r.ref, tr, r.keychain)
}

func (r *Blob) Size() int64 {
	return r.size
}

func (r *Blob) FetchedSize() int64 {
	r.fetchedRegionSetMu.Lock()
	sz := r.fetchedRegionSet.totalSize()
	r.fetchedRegionSetMu.Unlock()
	return sz
}

// ReadAt reads remote chunks from specified offset for the buffer size.
// It tries to fetch as many chunks as possible from local cache.
// We can configure this function with options.
func (r *Blob) ReadAt(p []byte, offset int64, opts ...Option) (int, error) {
	opt := options{}
	for _, o := range opts {
		o(&opt)
	}
	var (
		ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
		tr          = r.tr
	)
	defer cancel()
	if opt.ctx != nil {
		ctx = opt.ctx
	}
	if opt.tr != nil {
		tr = opt.tr
	}

	if len(p) == 0 || offset > r.size {
		return 0, nil
	}

	// Fetch all data.
	allRegion := region{floor(offset, r.chunkSize), ceil(offset+int64(len(p))-1, r.chunkSize) - 1}
	allData := map[region][]byte{}
	remotes := r.appendFromCache(allData, allRegion)
	if err := r.appendFromRemote(ctx, tr, allData, remotes); err != nil {
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
		return 0, errors.Wrapf(err, "failed to gather chunks for region (%d, %d)",
			allRegion.b, allRegion.e)
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

type walkFunc func(reg region) error

// walkChunks walks chunks from begin to end in order in the specified region.
func (r *Blob) walkChunks(allRegion region, walkFn walkFunc) error {
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

// appendFromRemote fetches all specified chunks from local cache.
func (r *Blob) appendFromCache(allData map[region][]byte, whole region) map[region]bool {
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
func (r *Blob) appendFromRemote(ctx context.Context, tr http.RoundTripper, allData map[region][]byte, requests map[region]bool) error {
	if len(requests) == 0 {
		return nil
	}

	// squash requesting chunks to reduce the total size of request header (servers
	// normally have limits for the size of headers)
	// TODO: when our request has too many ranges, we need to divide it into
	//       multiple requests to avoid huge header.
	rs := regionSet{}
	for reg := range requests {
		rs.add(reg)
	}

	// request specified ranges.
	req, err := http.NewRequest("GET", r.url, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to make GET request for %q", r.url)
	}
	req = req.WithContext(ctx)
	ranges := "bytes=0-0," // dummy range to make sure the response to be multipart
	for _, reg := range rs.rs {
		ranges += fmt.Sprintf("%d-%d,", reg.b, reg.e)
	}
	req.Header.Add("Range", ranges[:len(ranges)-1])
	req.Header.Add("Accept-Encoding", "identity")
	req.Close = false
	res, err := tr.RoundTrip(req) // NOT DefaultClient; don't want redirects
	if err != nil {
		return errors.Wrapf(err, "failed to request to %q", r.url)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code on %q: %v", r.url, res.Status)
	}

	// If We get whole blob in one part(= status 200), we chunk and return them.
	if res.StatusCode == http.StatusOK {
		data, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return errors.Wrapf(err, "Failed to read response body from %q", r.url)
		}
		gotSize := int64(len(data))
		requiredSize := int64(0)
		for reg := range requests {
			allData[reg] = data[reg.b : reg.e+1]
			requiredSize += reg.e - reg.b + 1
		}
		if requiredSize != gotSize {
			return fmt.Errorf("broken response body for %q; want size %d but got %d",
				r.url, requiredSize, gotSize)
		}
		return nil
	}

	// Get all chunks responsed as a multipart body.
	mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return errors.Wrapf(err, "invalid media type %q for %q", mediaType, r.url)
	}
	mr := multipart.NewReader(res.Body, params["boundary"])
	mr.NextPart() // Drop the dummy range.
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		} else if err != nil {
			return errors.Wrapf(err, "failed to read multipart response from %q", r.url)
		}
		reg, err := r.parseRange(p.Header.Get("Content-Range"))
		if err != nil {
			return errors.Wrapf(err, "failed to parse Content-Range header of %q", r.url)
		}
		data, err := ioutil.ReadAll(p)
		if err != nil {
			return errors.Wrapf(err, "failed to read multipart response data on %q", r.url)
		}

		// Chunk this part
		if err := r.walkChunks(reg, func(chunk region) error {
			var (
				base = chunk.b - reg.b
				end  = base + chunk.size()
			)
			if int64(len(data)) < end {
				return fmt.Errorf("invalid part data on %q: size %d; want at least %d",
					r.url, len(data), end)
			}
			allData[chunk] = data[base:end]
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func (r *Blob) parseRange(header string) (region, error) {
	submatches := contentRangeRegexp.FindStringSubmatch(header)
	if len(submatches) < 4 {
		return region{}, fmt.Errorf("Content-Range %q doesn't have enough information", header)
	}
	begin, err := strconv.ParseInt(submatches[1], 10, 64)
	if err != nil {
		return region{}, errors.Wrapf(err, "failed to parse beginning offset %q", submatches[1])
	}
	end, err := strconv.ParseInt(submatches[2], 10, 64)
	if err != nil {
		return region{}, errors.Wrapf(err, "failed to parse end offset %q", submatches[2])
	}

	return region{begin, end}, nil
}

func (r *Blob) genID(reg region) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", r.url, reg.b, reg.e)))
	return fmt.Sprintf("%x", sum)
}

func floor(n int64, unit int64) int64 {
	return (n / unit) * unit
}

func ceil(n int64, unit int64) int64 {
	return (n/unit + 1) * unit
}

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

// region is HTTP-range-request-compliant range.
// "b" is beginning byte of the range and "e" is the end.
// "e" is must be inclusive along with HTTP's range expression.
type region struct{ b, e int64 }

func (c region) size() int64 {
	return c.e - c.b + 1
}

type Option func(*options)

type options struct {
	ctx context.Context
	tr  http.RoundTripper
}

func WithContext(ctx context.Context) Option {
	return func(opts *options) {
		opts.ctx = ctx
	}
}

func WithRoundTripper(tr http.RoundTripper) Option {
	return func(opts *options) {
		opts.tr = tr
	}
}
