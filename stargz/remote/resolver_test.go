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
	"strings"
	"testing"

	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
)

func TestMirror(t *testing.T) {
	ref := "dummyexample.com/library/test"
	refspec, err := reference.Parse(ref)
	if err != nil {
		t.Fatalf("failed to prepare dummy reference: %v", err)
	}
	var (
		blobDigest = "sha256:deadbeaf"
		blobPath   = fmt.Sprintf("/v2/%s/blobs/%s",
			strings.TrimPrefix(refspec.Locator, refspec.Hostname()+"/"), blobDigest)
		refHost = refspec.Hostname()
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
			var hosts []RegistryHost
			for _, m := range append(tt.mirrors, refHost) {
				hosts = append(hosts, RegistryHost{
					RegistryHost: docker.RegistryHost{
						Client:       &http.Client{Transport: tt.tr},
						Host:         m,
						Scheme:       "https",
						Path:         "/v2",
						Capabilities: docker.HostCapabilityPull,
					},
				})
			}
			for i := range hosts {
				hosts[i].RefAlias = refspec
			}
			fetcher, _, err := newFetcher(hosts, blobDigest)
			if err != nil {
				if tt.error {
					return
				}
				t.Fatalf("failed to resolve reference: %v", err)
			}
			nurl, err := url.Parse(fetcher.url)
			if err != nil {
				t.Fatalf("failed to parse url %q: %v", fetcher.url, err)
			}
			if nurl.Hostname() != tt.wantHost {
				t.Errorf("invalid hostname %q(%q); want %q",
					nurl.Hostname(), nurl.String(), tt.wantHost)
			}
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
				Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
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
				Body:       ioutil.NopCloser(bytes.NewReader([]byte{0})),
				Request:    req,
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       ioutil.NopCloser(bytes.NewReader([]byte{})),
		Request:    req,
	}, nil
}

func TestCheck(t *testing.T) {
	tr := &breakRoundTripper{}
	f := &fetcher{
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
