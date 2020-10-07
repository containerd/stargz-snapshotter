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
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/stargz/config"
	"github.com/pkg/errors"
)

const (
	defaultChunkSize        = 50000
	defaultValidIntervalSec = 60
)

// RegistryHosts returns list of docker.RegistryHost based on the host name and
// labels.
type RegistryHosts func(host string, labels map[string]string) ([]docker.RegistryHost, error)

func NewResolver(hosts RegistryHosts, cache cache.BlobCache, cfg config.BlobConfig) *Resolver {
	if cfg.ChunkSize == 0 { // zero means "use default chunk size"
		cfg.ChunkSize = defaultChunkSize
	}
	if cfg.ValidInterval == 0 { // zero means "use default interval"
		cfg.ValidInterval = defaultValidIntervalSec
	}
	if cfg.CheckAlways {
		cfg.ValidInterval = 0
	}

	return &Resolver{
		hosts: hosts,
		bufPool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
		blobCache:  cache,
		blobConfig: cfg,
	}
}

type Resolver struct {
	hosts      RegistryHosts
	blobCache  cache.BlobCache
	blobConfig config.BlobConfig
	bufPool    sync.Pool
}

func (r *Resolver) Resolve(ref, digest string, labels map[string]string) (Blob, error) {
	refspec, err := reference.Parse(ref)
	if err != nil {
		return nil, err
	}
	hosts, err := r.hosts(refspec.Hostname(), labels)
	if err != nil {
		return nil, err
	}
	fetcher, size, err := newFetcher(hosts, refspec, digest)
	if err != nil {
		return nil, err
	}

	return &blob{
		fetcher:       fetcher,
		size:          size,
		chunkSize:     r.blobConfig.ChunkSize,
		cache:         r.blobCache,
		lastCheck:     time.Now(),
		checkInterval: time.Duration(r.blobConfig.ValidInterval) * time.Second,
		resolver:      r,
		refspec:       refspec,
		digest:        digest,
	}, nil
}

func newFetcher(hosts []docker.RegistryHost, refspec reference.Spec, digest string) (*fetcher, int64, error) {
	u, err := url.Parse("dummy://" + refspec.Locator)
	if err != nil {
		return nil, 0, err
	}
	// Try to create fetcher until succeeded
	rErr := fmt.Errorf("failed to resolve")
	for _, h := range hosts {
		if h.Host == "" || strings.Contains(h.Host, "/") {
			rErr = errors.Wrapf(rErr, "host %q: mirror must be a domain name", h.Host)
			continue // Try another host
		}

		// Prepare transport with authorization functionality
		tr := h.Client.Transport
		if h.Authorizer != nil {
			tr = &transport{
				inner: tr,
				auth:  h.Authorizer,
				// Specify pull scope
				// TODO: The scope generator function in containerd (github.com/containerd/containerd/remotes/docker/scope.go) should be exported and used here.
				scope: "repository:" + strings.TrimPrefix(u.Path, "/") + ":pull",
			}
		}

		// Resolve redirection and get blob URL
		url, err := redirect(fmt.Sprintf("%s://%s/%s/blobs/%s",
			h.Scheme,
			path.Join(h.Host, h.Path),
			strings.TrimPrefix(refspec.Locator, refspec.Hostname()+"/"),
			digest), tr)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q (ref:%q, digest:%q): failed to redirect: %v",
				h.Host, refspec, digest, err)
			continue // Try another host
		}

		// Get size information
		size, err := getSize(url, tr)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to get size: %v", h.Host, err)
			continue // Try another host
		}

		// Hit one host
		return &fetcher{
			url: url,
			tr:  tr,
		}, size, nil
	}

	return nil, 0, errors.Wrapf(rErr, "cannot resolve ref %q (%q)", refspec, digest)
}

type transport struct {
	inner http.RoundTripper
	auth  docker.Authorizer
	scope string
}

func (tr *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := docker.WithScope(req.Context(), tr.scope)
	roundTrip := func(req *http.Request) (*http.Response, error) {
		// authorize the request using docker.Authorizer
		if err := tr.auth.Authorize(ctx, req); err != nil {
			return nil, err
		}

		// send the request
		return tr.inner.RoundTrip(req)
	}

	resp, err := roundTrip(req)
	if err != nil {
		return nil, err
	}

	// TODO: support more status codes and retries
	if resp.StatusCode == http.StatusUnauthorized {

		// prepare authorization for the target host using docker.Authorizer
		if err := tr.auth.AddResponses(ctx, []*http.Response{resp}); err != nil {
			if errdefs.IsNotImplemented(err) {
				return resp, nil
			}
			return nil, err
		}

		// re-authorize and send the request
		return roundTrip(req.Clone(ctx))
	}

	return resp, nil
}

func redirect(endpointURL string, tr http.RoundTripper) (url string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// We use GET request for GCR.
	req, err := http.NewRequestWithContext(ctx, "GET", endpointURL, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to make request to the registry")
	}
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := tr.RoundTrip(req)
	if err != nil {
		return "", errors.Wrapf(err, "failed to request")
	}
	defer func() {
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()

	if res.StatusCode/100 == 2 {
		url = endpointURL
	} else if redir := res.Header.Get("Location"); redir != "" && res.StatusCode/100 == 3 {
		// TODO: Support nested redirection
		url = redir
	} else {
		return "", fmt.Errorf("failed to access to the registry with code %v", res.StatusCode)
	}

	return
}

func getSize(url string, tr http.RoundTripper) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return 0, err
	}
	req.Close = false
	res, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed HEAD request with code %v", res.StatusCode)
	}
	return strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
}

type fetcher struct {
	url           string
	tr            http.RoundTripper
	singleRange   bool
	singleRangeMu sync.Mutex
}

type multipartReadCloser interface {
	Next() (region, io.Reader, error)
	Close() error
}

func (f *fetcher) fetch(ctx context.Context, rs []region, opts *options) (multipartReadCloser, error) {
	if len(rs) == 0 {
		return nil, fmt.Errorf("no request queried")
	}

	var (
		tr              = f.tr
		singleRangeMode = f.isSingleRangeMode()
	)

	if opts.ctx != nil {
		ctx = opts.ctx
	}
	if opts.tr != nil {
		tr = opts.tr
	}

	// squash requesting chunks for reducing the total size of request header
	// (servers generally have limits for the size of headers)
	// TODO: when our request has too many ranges, we need to divide it into
	//       multiple requests to avoid huge header.
	var s regionSet
	for _, reg := range rs {
		s.add(reg)
	}
	requests := s.rs
	if singleRangeMode {
		// Squash requests if the layer doesn't support multi range.
		requests = []region{superRegion(requests)}
	}

	// Request to the registry
	req, err := http.NewRequestWithContext(ctx, "GET", f.url, nil)
	if err != nil {
		return nil, err
	}
	var ranges string
	for _, reg := range requests {
		ranges += fmt.Sprintf("%d-%d,", reg.b, reg.e)
	}
	req.Header.Add("Range", fmt.Sprintf("bytes=%s", ranges[:len(ranges)-1]))
	req.Header.Add("Accept-Encoding", "identity")
	req.Close = false
	res, err := tr.RoundTrip(req) // NOT DefaultClient; don't want redirects
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusOK {
		// We are getting the whole blob in one part (= status 200)
		size, err := strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse Content-Length")
		}
		return singlePartReader(region{0, size - 1}, res.Body), nil
	} else if res.StatusCode == http.StatusPartialContent {
		mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if err != nil {
			return nil, errors.Wrapf(err, "invalid media type %q", mediaType)
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			// We are getting a set of chunks as a multipart body.
			return multiPartReader(res.Body, params["boundary"]), nil
		}

		// We are getting single range
		reg, err := parseRange(res.Header.Get("Content-Range"))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse Content-Range")
		}
		return singlePartReader(reg, res.Body), nil
	} else if !singleRangeMode {
		f.singleRangeMode()           // fallbacks to singe range request mode
		return f.fetch(ctx, rs, opts) // retries with the single range mode
	}

	return nil, fmt.Errorf("unexpected status code: %v", res.Status)
}

func (f *fetcher) check() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", f.url, nil)
	if err != nil {
		return errors.Wrapf(err, "check failed: failed to make request")
	}
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := f.tr.RoundTrip(req)
	if err != nil {
		return errors.Wrapf(err, "check failed: failed to request to registry")
	}
	defer func() {
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code %v", res.StatusCode)
	}

	return nil
}

func (f *fetcher) genID(reg region) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d-%d", f.url, reg.b, reg.e)))
	return fmt.Sprintf("%x", sum)
}

func (f *fetcher) singleRangeMode() {
	f.singleRangeMu.Lock()
	f.singleRange = true
	f.singleRangeMu.Unlock()
}

func (f *fetcher) isSingleRangeMode() bool {
	f.singleRangeMu.Lock()
	r := f.singleRange
	f.singleRangeMu.Unlock()
	return r
}

func singlePartReader(reg region, rc io.ReadCloser) multipartReadCloser {
	return &singlepartReader{
		r:      rc,
		Closer: rc,
		reg:    reg,
	}
}

type singlepartReader struct {
	io.Closer
	r      io.Reader
	reg    region
	called bool
}

func (sr *singlepartReader) Next() (region, io.Reader, error) {
	if !sr.called {
		sr.called = true
		return sr.reg, sr.r, nil
	}
	return region{}, nil, io.EOF
}

func multiPartReader(rc io.ReadCloser, boundary string) multipartReadCloser {
	return &multipartReader{
		m:      multipart.NewReader(rc, boundary),
		Closer: rc,
	}
}

type multipartReader struct {
	io.Closer
	m *multipart.Reader
}

func (sr *multipartReader) Next() (region, io.Reader, error) {
	p, err := sr.m.NextPart()
	if err != nil {
		return region{}, nil, err
	}
	reg, err := parseRange(p.Header.Get("Content-Range"))
	if err != nil {
		return region{}, nil, errors.Wrapf(err, "failed to parse Content-Range")
	}
	return reg, p, nil
}

func parseRange(header string) (region, error) {
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

type Option func(*options)

type options struct {
	ctx       context.Context
	tr        http.RoundTripper
	cacheOpts []cache.Option
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

func WithCacheOpts(cacheOpts ...cache.Option) Option {
	return func(opts *options) {
		opts.cacheOpts = cacheOpts
	}
}
