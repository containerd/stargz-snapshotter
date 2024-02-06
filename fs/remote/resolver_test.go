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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/stargz-snapshotter/fs/source"
	rhttp "github.com/hashicorp/go-retryablehttp"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestMirror(t *testing.T) {
	ref := "dummyexample.com/library/test"
	refspec, err := reference.Parse(ref)
	if err != nil {
		t.Fatalf("failed to prepare dummy reference: %v", err)
	}
	var (
		blobDigest = digest.FromString("dummy")
		blobPath   = fmt.Sprintf("/v2/%s/blobs/%s",
			strings.TrimPrefix(refspec.Locator, refspec.Hostname()+"/"), blobDigest.String())
		refHost = refspec.Hostname()
	)

	tests := []struct {
		name     string
		hosts    func(t *testing.T) source.RegistryHosts
		wantHost string
		error    bool
	}{
		{
			name: "no-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{okURLs: []string{refHost}},
			),
			wantHost: refHost,
		},
		{
			name: "valid-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{okURLs: []string{"mirrorexample.com"}},
				hostSimple("mirrorexample.com"),
			),
			wantHost: "mirrorexample.com",
		},
		{
			name: "invalid-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{
					withCode: map[string]int{
						"mirrorexample1.com": http.StatusInternalServerError,
						"mirrorexample2.com": http.StatusUnauthorized,
						"mirrorexample3.com": http.StatusNotFound,
					},
					okURLs: []string{"mirrorexample4.com", refHost},
				},
				hostSimple("mirrorexample1.com"),
				hostSimple("mirrorexample2.com"),
				hostSimple("mirrorexample3.com"),
				hostSimple("mirrorexample4.com"),
			),
			wantHost: "mirrorexample4.com",
		},
		{
			name: "invalid-all-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{
					withCode: map[string]int{
						"mirrorexample1.com": http.StatusInternalServerError,
						"mirrorexample2.com": http.StatusUnauthorized,
						"mirrorexample3.com": http.StatusNotFound,
					},
					okURLs: []string{refHost},
				},
				hostSimple("mirrorexample1.com"),
				hostSimple("mirrorexample2.com"),
				hostSimple("mirrorexample3.com"),
			),
			wantHost: refHost,
		},
		{
			name: "invalid-hostname-of-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{
					okURLs: []string{`.*`},
				},
				hostSimple("mirrorexample.com/somepath/"),
			),
			wantHost: refHost,
		},
		{
			name: "redirected-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{
					redirectURL: map[string]string{
						regexp.QuoteMeta(fmt.Sprintf("mirrorexample.com%s", blobPath)): "https://backendexample.com/blobs/" + blobDigest.String(),
					},
					okURLs: []string{`.*`},
				},
				hostSimple("mirrorexample.com"),
			),
			wantHost: "backendexample.com",
		},
		{
			name: "invalid-redirected-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{
					withCode: map[string]int{
						"backendexample.com": http.StatusInternalServerError,
					},
					redirectURL: map[string]string{
						regexp.QuoteMeta(fmt.Sprintf("mirrorexample.com%s", blobPath)): "https://backendexample.com/blobs/" + blobDigest.String(),
					},
					okURLs: []string{`.*`},
				},
				hostSimple("mirrorexample.com"),
			),
			wantHost: refHost,
		},
		{
			name: "fail-all",
			hosts: hostsConfig(
				&sampleRoundTripper{},
				hostSimple("mirrorexample.com"),
			),
			wantHost: "",
			error:    true,
		},
		{
			name: "headers",
			hosts: hostsConfig(
				&sampleRoundTripper{
					okURLs: []string{`.*`},
					wantHeaders: map[string]http.Header{
						"mirrorexample.com": http.Header(map[string][]string{
							"test-a-key": {"a-value-1", "a-value-2"},
							"test-b-key": {"b-value-1"},
						}),
					},
				},
				hostWithHeaders("mirrorexample.com", map[string][]string{
					"test-a-key": {"a-value-1", "a-value-2"},
					"test-b-key": {"b-value-1"},
				}),
			),
			wantHost: "mirrorexample.com",
		},
		{
			name: "headers-with-mirrors",
			hosts: hostsConfig(
				&sampleRoundTripper{
					withCode: map[string]int{
						"mirrorexample1.com": http.StatusInternalServerError,
						"mirrorexample2.com": http.StatusInternalServerError,
					},
					okURLs: []string{"mirrorexample3.com", refHost},
					wantHeaders: map[string]http.Header{
						"mirrorexample1.com": http.Header(map[string][]string{
							"test-a-key": {"a-value"},
						}),
						"mirrorexample2.com": http.Header(map[string][]string{
							"test-b-key":   {"b-value"},
							"test-b-key-2": {"b-value-2", "b-value-3"},
						}),
						"mirrorexample3.com": http.Header(map[string][]string{
							"test-c-key": {"c-value"},
						}),
					},
				},
				hostWithHeaders("mirrorexample1.com", map[string][]string{
					"test-a-key": {"a-value"},
				}),
				hostWithHeaders("mirrorexample2.com", map[string][]string{
					"test-b-key":   {"b-value"},
					"test-b-key-2": {"b-value-2", "b-value-3"},
				}),
				hostWithHeaders("mirrorexample3.com", map[string][]string{
					"test-c-key": {"c-value"},
				}),
			),
			wantHost: "mirrorexample3.com",
		},
		{
			name: "headers-with-mirrors-invalid-all",
			hosts: hostsConfig(
				&sampleRoundTripper{
					withCode: map[string]int{
						"mirrorexample1.com": http.StatusInternalServerError,
						"mirrorexample2.com": http.StatusInternalServerError,
					},
					okURLs: []string{"mirrorexample3.com", refHost},
					wantHeaders: map[string]http.Header{
						"mirrorexample1.com": http.Header(map[string][]string{
							"test-a-key": {"a-value"},
						}),
						"mirrorexample2.com": http.Header(map[string][]string{
							"test-b-key":   {"b-value"},
							"test-b-key-2": {"b-value-2", "b-value-3"},
						}),
					},
				},
				hostWithHeaders("mirrorexample1.com", map[string][]string{
					"test-a-key": {"a-value"},
				}),
				hostWithHeaders("mirrorexample2.com", map[string][]string{
					"test-b-key":   {"b-value"},
					"test-b-key-2": {"b-value-2", "b-value-3"},
				}),
			),
			wantHost: refHost,
		},
		{
			name: "headers-with-redirected-mirror",
			hosts: hostsConfig(
				&sampleRoundTripper{
					redirectURL: map[string]string{
						regexp.QuoteMeta(fmt.Sprintf("mirrorexample.com%s", blobPath)): "https://backendexample.com/blobs/" + blobDigest.String(),
					},
					okURLs: []string{`.*`},
					wantHeaders: map[string]http.Header{
						"mirrorexample.com": http.Header(map[string][]string{
							"test-a-key":   {"a-value"},
							"test-b-key-2": {"b-value-2", "b-value-3"},
						}),
					},
				},
				hostWithHeaders("mirrorexample.com", map[string][]string{
					"test-a-key":   {"a-value"},
					"test-b-key-2": {"b-value-2", "b-value-3"},
				}),
			),
			wantHost: "backendexample.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetcher, _, err := newHTTPFetcher(context.Background(), &fetcherConfig{
				hosts:   tt.hosts(t),
				refspec: refspec,
				desc:    ocispec.Descriptor{Digest: blobDigest},
			})
			if err != nil {
				if tt.error {
					return
				}
				t.Fatalf("failed to resolve reference: %v", err)
			}
			checkFetcherURL(t, fetcher, tt.wantHost)

			// Test check()
			if err := fetcher.check(); err != nil {
				t.Fatalf("failed to check fetcher: %v", err)
			}

			// Test refreshURL()
			if err := fetcher.refreshURL(context.TODO()); err != nil {
				t.Fatalf("failed to refresh URL: %v", err)
			}
			checkFetcherURL(t, fetcher, tt.wantHost)
		})
	}
}

func checkFetcherURL(t *testing.T, f *httpFetcher, wantHost string) {
	nurl, err := url.Parse(f.url)
	if err != nil {
		t.Fatalf("failed to parse url %q: %v", f.url, err)
	}
	if nurl.Hostname() != wantHost {
		t.Errorf("invalid hostname %q(%q); want %q", nurl.Hostname(), nurl.String(), wantHost)
	}
}

type sampleRoundTripper struct {
	t           *testing.T
	withCode    map[string]int
	redirectURL map[string]string
	okURLs      []string
	wantHeaders map[string]http.Header
}

func getTestHeaders(headers map[string][]string) map[string][]string {
	res := make(map[string][]string)
	for k, v := range headers {
		if strings.HasPrefix(k, "test-") {
			res[k] = v
		}
	}
	return res
}

func (tr *sampleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	reqHeader := getTestHeaders(req.Header)
	for host, wHeaders := range tr.wantHeaders {
		wantHeader := getTestHeaders(wHeaders)
		if ok, _ := regexp.Match(host, []byte(req.URL.String())); ok {
			if len(wantHeader) != len(reqHeader) {
				tr.t.Fatalf("unexpected num of headers; got %d, wanted %d", len(wantHeader), len(reqHeader))
			}
			for k, v := range wantHeader {
				gotV, ok := reqHeader[k]
				if !ok {
					tr.t.Fatalf("required header %q not found; got %+v", k, reqHeader)
				}
				wantVM := make(map[string]struct{})
				for _, e := range v {
					wantVM[e] = struct{}{}
				}
				if len(gotV) != len(v) {
					tr.t.Fatalf("unexpected num of header values of %q; got %d, wanted %d", k, len(gotV), len(v))
				}
				for _, gotE := range gotV {
					delete(wantVM, gotE)
				}
				if len(wantVM) != 0 {
					tr.t.Fatalf("header %q must have elements %+v", k, wantVM)
				}
				delete(reqHeader, k)
			}
		}
	}
	if len(reqHeader) != 0 {
		tr.t.Fatalf("unexpected headers %+v", reqHeader)
	}

	for host, code := range tr.withCode {
		if ok, _ := regexp.Match(host, []byte(req.URL.String())); ok {
			return &http.Response{
				StatusCode: code,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader([]byte{})),
				Request:    req,
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
				Body:       io.NopCloser(bytes.NewReader([]byte{})),
				Request:    req,
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
				Body:       io.NopCloser(bytes.NewReader([]byte{0})),
				Request:    req,
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader([]byte{})),
		Request:    req,
	}, nil
}

func TestCheck(t *testing.T) {
	tr := &breakRoundTripper{}
	f := &httpFetcher{
		url: "test",
		tr:  tr,
	}
	tr.success = true
	if err := f.check(); err != nil {
		t.Errorf("connection failed; wanted to succeed")
	}

	tr.success = false
	if err := f.check(); err == nil {
		t.Errorf("connection succeeded; wanted to fail")
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
			Body:       io.NopCloser(bytes.NewReader([]byte("test"))),
		}
	} else {
		res = &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte{})),
		}
	}
	return
}

func TestRetry(t *testing.T) {
	tr := &retryRoundTripper{}
	rclient := rhttp.NewClient()
	rclient.HTTPClient.Transport = tr
	rclient.Backoff = backoffStrategy
	f := &httpFetcher{
		url: "test",
		tr:  &rhttp.RoundTripper{Client: rclient},
	}

	regions := []region{{b: 0, e: 1}}

	_, err := f.fetch(context.Background(), regions, true)

	if err != nil {
		t.Fatalf("unexpected error = %v", err)
	}

	if tr.retryCount != 4 {
		t.Fatalf("unxpected retryCount; expected=4 got=%d", tr.retryCount)
	}
}

type retryRoundTripper struct {
	retryCount int
}

func (r *retryRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	defer func() {
		r.retryCount++
	}()

	switch r.retryCount {
	case 0:
		err = fmt.Errorf("dummy error")
	case 1:
		res = &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte{})),
		}
	case 2:
		res = &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte{})),
		}
	default:
		header := make(http.Header)
		header.Add("Content-Length", "4")
		res = &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader([]byte("test"))),
		}
	}
	return
}

type hostFactory func(tr http.RoundTripper) docker.RegistryHost

func hostSimple(host string) hostFactory {
	return func(tr http.RoundTripper) docker.RegistryHost {
		return docker.RegistryHost{
			Client:       &http.Client{Transport: tr},
			Host:         host,
			Scheme:       "https",
			Path:         "/v2",
			Capabilities: docker.HostCapabilityPull,
		}
	}
}

func hostWithHeaders(host string, headers http.Header) hostFactory {
	return func(tr http.RoundTripper) docker.RegistryHost {
		return docker.RegistryHost{
			Client:       &http.Client{Transport: tr},
			Host:         host,
			Scheme:       "https",
			Path:         "/v2",
			Capabilities: docker.HostCapabilityPull,
			Header:       headers,
		}
	}
}

func hostsConfig(tr *sampleRoundTripper, mirrors ...hostFactory) func(t *testing.T) source.RegistryHosts {
	return func(t *testing.T) source.RegistryHosts {
		tr.t = t
		return func(refspec reference.Spec) (reghosts []docker.RegistryHost, _ error) {
			host := refspec.Hostname()
			for _, m := range mirrors {
				reghosts = append(reghosts, m(tr))
			}
			reghosts = append(reghosts, hostSimple(host)(tr))
			return
		}
	}
}
