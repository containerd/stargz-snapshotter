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

package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
)

const expiredECRBody = `{"errors":[{"code":"DENIED","message":"Your authorization token has expired. Reauthenticate and try again."}]}`

func TestShouldAuthenticate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"unauthorized", http.StatusUnauthorized, "", true},
		{"expired-ecr", http.StatusForbidden, expiredECRBody, true},
		{"permission-denied", http.StatusForbidden, `{"errors":[{"code":"DENIED","message":"permission denied"}]}`, false},
		{"expired-object-url", http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>`, false},
		{"invalid-json", http.StatusForbidden, `{"errors":`, false},
		{"oversized", http.StatusForbidden, expiredECRBody + strings.Repeat(" ", 64*1024), false},
		{"success", http.StatusOK, expiredECRBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "https://registry.example/v2/repo/blobs/digest", nil)
			body := &trackedAuthBody{ReadCloser: io.NopCloser(strings.NewReader(tc.body))}
			resp := &http.Response{StatusCode: tc.status, Body: body, Request: req}
			if got := shouldAuthenticate(resp); got != tc.want {
				t.Fatalf("shouldAuthenticate = %v, want %v", got, tc.want)
			}
			if tc.status == http.StatusForbidden && tc.want && !strings.HasPrefix(resp.Header.Get("Www-Authenticate"), "Basic ") {
				t.Fatal("missing Basic challenge")
			}
			gotBody, err := io.ReadAll(resp.Body)
			if err != nil || string(gotBody) != tc.body {
				t.Fatalf("body was not preserved: %q, %v", gotBody, err)
			}
			resp.Body.Close()
			if !body.closed.Load() {
				t.Fatal("original body was not closed")
			}
		})
	}

	req, _ := http.NewRequest(http.MethodGet, "https://registry.example/", nil)
	resp := &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(expiredECRBody)), Request: req, Header: http.Header{"Www-Authenticate": {`Basic realm="existing"`}}}
	defer resp.Body.Close()
	if !shouldAuthenticate(resp) || resp.Header.Get("Www-Authenticate") != `Basic realm="existing"` {
		t.Fatal("existing challenge was overwritten")
	}
	if shouldAuthenticate(&http.Response{StatusCode: http.StatusForbidden}) {
		t.Fatal("empty response triggered authentication")
	}
}

func TestTransportExpiredECRCredentials(t *testing.T) {
	for _, tc := range []struct {
		name            string
		denyFresh       bool
		credentialError bool
	}{
		{name: "renewed"},
		{name: "retry-bounded", denyFresh: true},
		{name: "credential-error", credentialError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, lookups atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				user, password, ok := r.BasicAuth()
				if !ok || user != "AWS" || (password != "old" && password != "fresh") {
					t.Error("request did not carry expected Basic credentials")
				}
				if password == "old" || tc.denyFresh {
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, expiredECRBody)
					return
				}
				io.WriteString(w, "blob data")
			}))
			defer server.Close()
			auth := docker.NewDockerAuthorizer(docker.WithAuthCreds(func(string) (string, string, error) {
				if lookups.Add(1) == 1 {
					return "AWS", "old", nil
				}
				if tc.credentialError {
					return "", "", errors.New("credential source unavailable")
				}
				return "AWS", "fresh", nil
			}))
			req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
			if err := auth.AddResponses(context.Background(), []*http.Response{{Request: req, Header: http.Header{"Www-Authenticate": {`Basic realm="registry"`}}, StatusCode: http.StatusUnauthorized}}); err != nil {
				t.Fatal(err)
			}
			var originalBody *trackedAuthBody
			tr := &transport{auth: auth, inner: authRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp, err := http.DefaultTransport.RoundTrip(r)
				if err == nil && originalBody == nil {
					originalBody = &trackedAuthBody{ReadCloser: resp.Body}
					resp.Body = originalBody
				}
				return resp, err
			})}
			resp, err := tr.RoundTrip(req)
			if originalBody == nil || !originalBody.closed.Load() {
				t.Fatal("discarded expiry response body was not closed")
			}
			if tc.credentialError {
				if err == nil || requests.Load() != 1 {
					t.Fatalf("credential error = %v, requests = %d", err, requests.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			wantStatus, wantBody := http.StatusOK, "blob data"
			if tc.denyFresh {
				wantStatus, wantBody = http.StatusForbidden, expiredECRBody
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != wantStatus || string(body) != wantBody || requests.Load() != 2 || lookups.Load() != 2 {
				t.Fatalf("status=%d body=%q error=%v requests=%d lookups=%d", resp.StatusCode, body, err, requests.Load(), lookups.Load())
			}
		})
	}
}

func TestTransportConcurrentECRRefresh(t *testing.T) {
	var lookups atomic.Int32
	auth := docker.NewDockerAuthorizer(docker.WithAuthCreds(func(string) (string, string, error) {
		if lookups.Add(1) == 1 {
			return "AWS", "old", nil
		}
		return "AWS", "fresh", nil
	}))
	newRequest := func() *http.Request {
		req, _ := http.NewRequest(http.MethodGet, "https://registry.example/v2/repo/blobs/digest", nil)
		return req
	}
	if err := auth.AddResponses(context.Background(), []*http.Response{{Request: newRequest(), Header: http.Header{"Www-Authenticate": {`Basic realm="registry"`}}, StatusCode: http.StatusUnauthorized}}); err != nil {
		t.Fatal(err)
	}
	tr := &transport{auth: auth, inner: authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		_, password, _ := req.BasicAuth()
		status, body := http.StatusOK, "blob data"
		if password != "fresh" {
			status, body = http.StatusForbidden, expiredECRBody
		}
		return &http.Response{Request: req, StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			resp, err := tr.RoundTrip(newRequest())
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("refresh did not recover: status %d", resp.StatusCode)
			}
		})
	}
	wg.Wait()
	if lookups.Load() < 2 {
		t.Fatal("credential source was not consulted again")
	}
}

type trackedAuthBody struct {
	io.ReadCloser
	closed atomic.Bool
}

type authRoundTripFunc func(*http.Request) (*http.Response, error)

func (f authRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (b *trackedAuthBody) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}
