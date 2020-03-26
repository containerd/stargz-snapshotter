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
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestMirror(t *testing.T) {
	ref, err := name.ParseReference("dummyexample.com/library/test")
	if err != nil {
		t.Fatalf("failed to prepare dummy reference: %v", err)
	}
	var (
		blobDigest = "sha256:deadbeaf"
		blobPath   = fmt.Sprintf("/v2/%s/blobs/%s", ref.Context().RepositoryStr(), blobDigest)
		refHost    = ref.Context().RegistryStr()
	)

	tests := []struct {
		name     string
		tr       http.RoundTripper
		mirrors  []string
		wantHost string
		error    bool
	}{
		{
			name:     "no-mirror",
			tr:       &sampleRoundTripper{okURLs: []string{refHost}},
			mirrors:  nil,
			wantHost: refHost,
		},
		{
			name:     "valid-mirror",
			tr:       &sampleRoundTripper{okURLs: []string{"mirrorexample.com"}},
			mirrors:  []string{"mirrorexample.com"},
			wantHost: "mirrorexample.com",
		},
		{
			name: "invalid-mirror",
			tr: &sampleRoundTripper{
				withCode: map[string]int{
					"mirrorexample1.com": http.StatusInternalServerError,
					"mirrorexample2.com": http.StatusUnauthorized,
					"mirrorexample3.com": http.StatusNotFound,
				},
				okURLs: []string{"mirrorexample4.com", refHost},
			},
			mirrors: []string{
				"mirrorexample1.com",
				"mirrorexample2.com",
				"mirrorexample3.com",
				"mirrorexample4.com",
			},
			wantHost: "mirrorexample4.com",
		},
		{
			name: "invalid-all-mirror",
			tr: &sampleRoundTripper{
				withCode: map[string]int{
					"mirrorexample1.com": http.StatusInternalServerError,
					"mirrorexample2.com": http.StatusUnauthorized,
					"mirrorexample3.com": http.StatusNotFound,
				},
				okURLs: []string{refHost},
			},
			mirrors: []string{
				"mirrorexample1.com",
				"mirrorexample2.com",
				"mirrorexample3.com",
			},
			wantHost: refHost,
		},
		{
			name: "invalid-hostname-of-mirror",
			tr: &sampleRoundTripper{
				okURLs: []string{`.*`},
			},
			mirrors:  []string{"mirrorexample.com/somepath/"},
			wantHost: refHost,
		},
		{
			name: "redirected-mirror",
			tr: &sampleRoundTripper{
				redirectURL: map[string]string{
					regexp.QuoteMeta(fmt.Sprintf("mirrorexample.com%s", blobPath)): "https://backendexample.com/blobs/sha256:deadbeaf",
				},
				okURLs: []string{`.*`},
			},
			mirrors:  []string{"mirrorexample.com"},
			wantHost: "backendexample.com",
		},
		{
			name: "invalid-redirected-mirror",
			tr: &sampleRoundTripper{
				withCode: map[string]int{
					"backendexample.com": http.StatusInternalServerError,
				},
				redirectURL: map[string]string{
					regexp.QuoteMeta(fmt.Sprintf("mirrorexample.com%s", blobPath)): "https://backendexample.com/blobs/sha256:deadbeaf",
				},
				okURLs: []string{`.*`},
			},
			mirrors:  []string{"mirrorexample.com"},
			wantHost: refHost,
		},
		{
			name:     "fail-all",
			tr:       &sampleRoundTripper{},
			mirrors:  []string{"mirrorexample.com"},
			wantHost: "",
			error:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mirrors []MirrorConfig
			for _, m := range tt.mirrors {
				mirrors = append(mirrors, MirrorConfig{
					Host: m,
				})
			}
			r := &Resolver{
				transport: tt.tr,
				trPool:    make(map[string]http.RoundTripper),
				config: map[string]ResolverConfig{
					refHost: {
						Mirrors: mirrors,
					},
				},
			}
			blob, err := r.Resolve(ref.String(), blobDigest, nil, BlobConfig{})
			if err != nil {
				if tt.error {
					return
				}
				t.Fatalf("failed to resolve reference: %v", err)
			}
			nurl, err := url.Parse(blob.url)
			if err != nil {
				t.Fatalf("failed to parse url %q: %v", blob.url, err)
			}
			if nurl.Hostname() != tt.wantHost {
				t.Errorf("invalid hostname %q(%q); want %q",
					nurl.Hostname(), nurl.String(), tt.wantHost)
			}
		})
	}
}

func TestPool(t *testing.T) {
	ref, err := name.ParseReference("dummyexample.com/library/test")
	if err != nil {
		t.Fatalf("failed to prepare dummy reference: %v", err)
	}
	var (
		digest  = "sha256:deadbeaf"
		blobURL = fmt.Sprintf("%s://%s/v2/%s/blobs/%s",
			ref.Context().Registry.Scheme(),
			ref.Context().RegistryStr(),
			ref.Context().RepositoryStr(),
			digest)
		redirectedURL  = "https://backendexample.com/blobs/sha256:deadbeaf"
		okRoundTripper = &sampleRoundTripper{
			okURLs: []string{`.*`},
		}
		redirectRoundTripper = &sampleRoundTripper{
			redirectURL: map[string]string{
				regexp.QuoteMeta(blobURL): redirectedURL,
			},
			okURLs: []string{regexp.QuoteMeta(ref.Context().RegistryStr())},
		}
		unauthorizedRoundTripper = &sampleRoundTripper{
			withCode: map[string]int{
				regexp.QuoteMeta(ref.Context().RegistryStr()): http.StatusUnauthorized,
			},
			okURLs: []string{`.*`},
		}
		notFoundRoundTripper = &sampleRoundTripper{}
	)
	tests := []struct {
		name  string
		pool  http.RoundTripper
		base  http.RoundTripper
		check func(t *testing.T, pool http.RoundTripper, url string, err error)
	}{
		{
			name: "create-new-tr",
			pool: nil,
			base: okRoundTripper,
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err != nil {
					t.Errorf("want to success: %v", err)
					return
				}
				if pool == nil {
					t.Error("new RoundTripper isn't pooled")
					return
				}
				if url != blobURL {
					t.Errorf("invalid URL %q; want %q", url, blobURL)
					return
				}
			},
		},
		{
			name: "create-new-tr-with-redirect",
			pool: nil,
			base: redirectRoundTripper,
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err != nil {
					t.Errorf("want to success: %v", err)
					return
				}
				if pool == nil {
					t.Error("new RoundTripper isn't pooled")
					return
				}
				if url != redirectedURL {
					t.Errorf("invalid URL %q; want %q", url, redirectedURL)
					return
				}
			},
		},
		{
			name: "fail-to-create-new-tr-with-unauthorized",
			pool: nil,
			base: unauthorizedRoundTripper,
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err == nil {
					t.Error("want to fail")
					return
				}
			},
		},
		{
			name: "use-pooled-ok-tr",
			pool: okRoundTripper,
			base: notFoundRoundTripper, // won't be used
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err != nil {
					t.Errorf("want to success: %v", err)
					return
				}
				if pool != okRoundTripper {
					t.Error("got RoundTripper isn't same as pooled one")
					return
				}
				if url != blobURL {
					t.Errorf("invalid URL %q; want %q", url, blobURL)
					return
				}
			},
		},
		{
			name: "use-pooled-redirect-tr",
			pool: redirectRoundTripper,
			base: notFoundRoundTripper, // won't be used
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err != nil {
					t.Errorf("want to success: %v", err)
					return
				}
				if pool != redirectRoundTripper {
					t.Error("got RoundTripper isn't same as pooled one")
					return
				}
				if url != redirectedURL {
					t.Errorf("invalid URL %q; want %q", url, redirectedURL)
					return
				}
			},
		},
		{
			name: "refresh-unauthorized-tr",
			pool: unauthorizedRoundTripper,
			base: okRoundTripper,
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err != nil {
					t.Errorf("want to success: %v", err)
					return
				}
				if pool == unauthorizedRoundTripper {
					t.Error("RoundTripper must be refreshed")
					return
				}
				if url != blobURL {
					t.Errorf("invalid URL %q; want %q", url, blobURL)
					return
				}
			},
		},
		{
			name: "refresh-unauthorized-tr-with-redirect",
			pool: unauthorizedRoundTripper,
			base: redirectRoundTripper,
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err != nil {
					t.Errorf("want to success: %v", err)
					return
				}
				if pool == unauthorizedRoundTripper {
					t.Error("RoundTripper must be refreshed")
					return
				}
				if url != redirectedURL {
					t.Errorf("invalid URL %q; want %q", url, redirectedURL)
					return
				}
			},
		},
		{
			name: "fail-to-refresh",
			pool: unauthorizedRoundTripper,
			base: notFoundRoundTripper,
			check: func(t *testing.T, pool http.RoundTripper, url string, err error) {
				if err == nil {
					t.Error("want to fail")
					return
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Resolver{
				transport: tt.base,
				trPool:    make(map[string]http.RoundTripper),
			}
			if tt.pool != nil {
				r.trPool[ref.Name()] = tt.pool
			}
			url, err := r.resolveReference(ref, digest)
			if r.trPool == nil {
				t.Fatal("transport pool is empty")
			}
			tt.check(t, r.trPool[ref.Name()], url, err)
		})
	}
}

type sampleRoundTripper struct {
	withCode    map[string]int
	redirectURL map[string]string
	okURLs      []string
}

func (tr *sampleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for host, code := range tr.withCode {
		if ok, _ := regexp.Match(host, []byte(req.URL.String())); ok {
			return &http.Response{
				StatusCode: code,
				Header:     make(http.Header),
				Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
			}, nil
		}
	}
	for host, rurl := range tr.redirectURL {
		if ok, _ := regexp.Match(host, []byte(req.URL.String())); ok {
			header := make(http.Header)
			header.Add("Location", rurl)
			return &http.Response{
				StatusCode: http.StatusMovedPermanently,
				Header:     header,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
			}, nil
		}
	}
	for _, host := range tr.okURLs {
		if ok, _ := regexp.Match(host, []byte(req.URL.String())); ok {
			header := make(http.Header)
			header.Add("Content-Length", "1")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     header,
				Body:       ioutil.NopCloser(bytes.NewReader([]byte{0})),
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
	}, nil
}
