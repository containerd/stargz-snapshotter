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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/reference/docker"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/golang/groupcache/lru"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/pkg/errors"
)

const (
	defaultChunkSize     = 50000
	defaultValidInterval = 60 * time.Second
	defaultPoolEntry     = 3000
)

type ResolverConfig struct {
	Host                map[string]HostConfig `toml:"host"`
	ConnectionPoolEntry int                   `toml:"connection_pool_entry"`
}

type HostConfig struct {
	Mirrors []MirrorConfig `toml:"mirrors"`
}

type MirrorConfig struct {
	Host     string `toml:"host"`
	Insecure bool   `toml:"insecure"`
}

type BlobConfig struct {
	ValidInterval int64 `toml:"valid_interval"`
	CheckAlways   bool  `toml:"check_always"`
	ChunkSize     int64 `toml:"chunk_size"`
}

func NewResolver(keychain authn.Keychain, config ResolverConfig) *Resolver {
	if config.Host == nil {
		config.Host = make(map[string]HostConfig)
	}
	poolEntry := config.ConnectionPoolEntry
	if poolEntry == 0 {
		poolEntry = defaultPoolEntry
	}
	return &Resolver{
		transport: http.DefaultTransport,
		trPool:    lru.New(poolEntry),
		keychain:  keychain,
		config:    config,
	}
}

type Resolver struct {
	transport http.RoundTripper
	trPool    *lru.Cache
	trPoolMu  sync.Mutex
	keychain  authn.Keychain
	config    ResolverConfig
}

func (r *Resolver) Resolve(ref, digest string, cache cache.BlobCache, config BlobConfig) (Blob, error) {
	fetcher, size, err := r.resolve(ref, digest)
	if err != nil {
		return nil, err
	}
	var (
		chunkSize     int64
		checkInterval time.Duration
	)
	chunkSize = config.ChunkSize
	if chunkSize == 0 { // zero means "use default chunk size"
		chunkSize = defaultChunkSize
	}
	if config.ValidInterval == 0 { // zero means "use default interval"
		checkInterval = defaultValidInterval
	} else {
		checkInterval = time.Duration(config.ValidInterval) * time.Second
	}
	if config.CheckAlways {
		checkInterval = 0
	}
	return &blob{
		fetcher:       fetcher,
		size:          size,
		keychain:      r.keychain,
		chunkSize:     chunkSize,
		cache:         cache,
		lastCheck:     time.Now(),
		checkInterval: checkInterval,
	}, nil
}

func (r *Resolver) Refresh(target Blob) error {
	b, ok := target.(*blob)
	if !ok {
		return fmt.Errorf("invalid type of blob. must be *blob")
	}

	// refresh the fetcher
	b.fetcherMu.Lock()
	defer b.fetcherMu.Unlock()
	new, newSize, err := b.fetcher.refresh()
	if err != nil {
		return err
	} else if newSize != b.size {
		return fmt.Errorf("Invalid size of new blob %d; want %d", newSize, b.size)
	}

	// update the blob's fetcher with new one
	b.fetcher = new
	b.lastCheck = time.Now()

	return nil
}

func (r *Resolver) resolve(ref, digest string) (*fetcher, int64, error) {
	var (
		nref name.Reference
		url  string
		tr   http.RoundTripper
		size int64
	)
	named, err := docker.ParseDockerRef(ref)
	if err != nil {
		return nil, 0, err
	}
	hosts := append(r.config.Host[docker.Domain(named)].Mirrors, MirrorConfig{
		Host: docker.Domain(named),
	})
	rErr := fmt.Errorf("failed to resolve")
	for _, h := range hosts {
		// Parse reference
		if h.Host == "" || strings.Contains(h.Host, "/") {
			rErr = errors.Wrapf(rErr, "host %q: mirror must be a domain name", h.Host)
			continue // try another host
		}
		var opts []name.Option
		if h.Insecure {
			opts = append(opts, name.Insecure)
		}
		sref := fmt.Sprintf("%s/%s", h.Host, docker.Path(named))
		nref, err = name.ParseReference(sref, opts...)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to parse ref %q (%q): %v",
				h.Host, sref, digest, err)
			continue // try another host
		}

		// Resolve redirection and get blob URL
		url, tr, err = r.resolveReference(nref, digest)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to resolve ref %q (%q): %v",
				h.Host, nref.String(), digest, err)
			continue // try another host
		}

		// Get size information
		size, err = getSize(url, tr)
		if err != nil {
			rErr = errors.Wrapf(rErr, "host %q: failed to get size of %q: %v",
				h.Host, url, err)
			continue // try another host
		}

		rErr = nil // Hit one accessible mirror
		break
	}
	if rErr != nil {
		return nil, 0, errors.Wrapf(rErr, "cannot resolve ref %q (%q)", ref, digest)
	}

	return &fetcher{
		resolver: r,
		ref:      ref,
		digest:   digest,
		nref:     nref,
		url:      url,
		tr:       tr,
	}, size, nil
}

func (r *Resolver) resolveReference(ref name.Reference, digest string) (string, http.RoundTripper, error) {
	r.trPoolMu.Lock()
	defer r.trPoolMu.Unlock()

	// Construct endpoint URL from given ref
	endpointURL := fmt.Sprintf("%s://%s/v2/%s/blobs/%s",
		ref.Context().Registry.Scheme(),
		ref.Context().RegistryStr(),
		ref.Context().RepositoryStr(),
		digest)

	// Try to use cached transport (cahced per reference name)
	if tr, ok := r.trPool.Get(ref.Name()); ok {
		if url, err := redirect(endpointURL, tr.(http.RoundTripper)); err == nil {
			return url, tr.(http.RoundTripper), nil
		}
	}

	// Remove the stale transport from cache
	r.trPool.Remove(ref.Name())

	// transport is unavailable/expired so refresh the transport and try again
	tr, err := authnTransport(ref, r.transport, r.keychain)
	if err != nil {
		return "", nil, err
	}
	url, err := redirect(endpointURL, tr)
	if err != nil {
		return "", nil, err
	}

	// Update transports cache
	r.trPool.Add(ref.Name(), tr)

	return url, tr, nil
}

func redirect(endpointURL string, tr http.RoundTripper) (url string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// We use GET request for GCR.
	req, err := http.NewRequestWithContext(ctx, "GET", endpointURL, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to request to the registry of %q", endpointURL)
	}
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := tr.RoundTrip(req)
	if err != nil {
		return "", errors.Wrapf(err, "failed to request to %q", endpointURL)
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
		return "", fmt.Errorf("failed to access to %q with code %v",
			endpointURL, res.StatusCode)
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

func authnTransport(ref name.Reference, tr http.RoundTripper, keychain authn.Keychain) (http.RoundTripper, error) {
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	authn, err := keychain.Resolve(ref.Context())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve the reference %q", ref)
	}
	errCh := make(chan error)
	var rTr http.RoundTripper
	go func() {
		rTr, err = transport.New(
			ref.Context().Registry,
			authn,
			tr,
			[]string{ref.Scope(transport.PullScope)},
		)
		errCh <- err
	}()
	select {
	case err = <-errCh:
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("authentication timeout")
	}
	return rTr, err
}

type fetcher struct {
	resolver      *Resolver
	ref           string
	digest        string
	nref          name.Reference
	url           string
	tr            http.RoundTripper
	singleRange   bool
	singleRangeMu sync.Mutex
}

func (f *fetcher) refresh() (*fetcher, int64, error) {
	return f.resolver.resolve(f.ref, f.digest)
}

func (f *fetcher) fetch(requests []region, opts ...Option) (map[region][]byte, error) {
	if len(requests) == 0 {
		return nil, nil
	}

	var (
		tr              = f.tr
		ctx, cancel     = context.WithTimeout(context.Background(), 30*time.Second)
		singleRangeMode = f.isSingleRangeMode()
	)
	defer cancel()

	if singleRangeMode {
		// Squash requests if the layer doesn't support multi range.
		requests = []region{superRegion(requests)}
	}

	// Parse options
	var opt options
	for _, o := range opts {
		o(&opt)
	}
	if opt.ctx != nil {
		ctx = opt.ctx
	}
	if opt.tr != nil {
		tr = opt.tr
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
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		// We are getting the whole blob in one part (= status 200)
		data, err := ioutil.ReadAll(res.Body) // TODO: chunk data for saving memory
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to read response body from %q", f.url)
		}
		size, err := strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse Content-Length for %q", f.url)
		}
		if int64(len(data)) != size {
			return nil, errors.Wrapf(err, "broken response body:got size %d; want %d for %q",
				len(data), size, f.url)
		}
		remoteData := make(map[region][]byte)
		remoteData[region{0, size - 1}] = data
		return remoteData, nil
	} else if res.StatusCode == http.StatusPartialContent {
		mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if err != nil {
			return nil, errors.Wrapf(err, "invalid media type %q for %q", mediaType, f.url)
		}

		if !strings.HasPrefix(mediaType, "multipart/") {
			// We are getting partial content
			data, err := ioutil.ReadAll(res.Body) // TODO: chunk data for saving memory
			if err != nil {
				return nil, errors.Wrapf(err, "failed to read response from %q", f.url)
			}
			reg, err := parseRange(res.Header.Get("Content-Range"))
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse Content-Range: %q", f.url)
			}
			if int64(len(data)) != reg.size() {
				return nil, errors.Wrapf(err, "broken part resp: size %d; want %d: %q",
					len(data), reg.size(), f.url)
			}
			remoteData := make(map[region][]byte)
			remoteData[reg] = data
			return remoteData, nil
		}

		// We are getting a set of chunks as a multipart body.
		remoteData := make(map[region][]byte)
		mr := multipart.NewReader(res.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, errors.Wrapf(err, "failed to read multipart resp: %q", f.url)
			}
			reg, err := parseRange(p.Header.Get("Content-Range"))
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse Content-Range: %q", f.url)
			}
			data, err := ioutil.ReadAll(p)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to read multipart data: %q", f.url)
			}
			if int64(len(data)) != reg.size() {
				return nil, errors.Wrapf(err, "broken part body: size %d; want %d: %q",
					len(data), reg.size(), f.url)

			}
			remoteData[reg] = data
		}
		return remoteData, nil
	} else if !singleRangeMode {
		// Retry with the single range mode
		f.singleRangeMode() // fallback to singe range request mode
		return f.fetch(requests, opts...)
	}

	return nil, fmt.Errorf("unexpected status code on %q: %v", f.url, res.Status)
}

func (f *fetcher) check() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", f.url, nil)
	if err != nil {
		return errors.Wrapf(err, "check failed: failed to make request for %q", f.url)
	}
	req.Close = false
	req.Header.Set("Range", "bytes=0-1")
	res, err := f.tr.RoundTrip(req)
	if err != nil {
		return errors.Wrapf(err, "check failed: failed to request to registry %q", f.url)
	}
	defer func() {
		io.Copy(ioutil.Discard, res.Body)
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("unexpected status code %v for %q", res.StatusCode, f.url)
	}

	return nil
}

func (f *fetcher) authn(tr http.RoundTripper, keychain authn.Keychain) (http.RoundTripper, error) {
	return authnTransport(f.nref, tr, keychain)
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
