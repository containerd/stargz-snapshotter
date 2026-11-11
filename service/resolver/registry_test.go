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

package resolver

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// redirectingRegistry emulates a registry (e.g. Amazon ECR) that answers blob
// requests with a redirect to a separate storage host (e.g. a pre-signed S3
// URL). It counts the requests that reach each host.
type redirectingRegistry struct {
	registry     *httptest.Server
	storage      *httptest.Server
	registryHits atomic.Int64
	storageHits  atomic.Int64
	repo         string
	blob         []byte
	dgst         digest.Digest
}

func newRedirectingRegistry(t *testing.T, blobSize int) *redirectingRegistry {
	t.Helper()
	blob := make([]byte, blobSize)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	r := &redirectingRegistry{
		repo: "test/repo",
		blob: blob,
		dgst: digest.FromBytes(blob),
	}
	r.storage = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.storageHits.Add(1)
		if req.URL.Path != "/"+r.dgst.Encoded() {
			http.NotFound(w, req)
			return
		}
		// ServeContent supports single and multi-range requests.
		http.ServeContent(w, req, "", time.Time{}, bytes.NewReader(r.blob))
	}))
	t.Cleanup(r.storage.Close)
	r.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.registryHits.Add(1)
		if req.URL.Path != fmt.Sprintf("/v2/%s/blobs/%s", r.repo, r.dgst) {
			http.NotFound(w, req)
			return
		}
		http.Redirect(w, req, r.storage.URL+"/"+r.dgst.Encoded(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(r.registry.Close)
	return r
}

func (r *redirectingRegistry) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(r.registry.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host // 127.0.0.1:<port>; treated as localhost, so plain http is used
}

func (r *redirectingRegistry) refspec(t *testing.T) reference.Spec {
	t.Helper()
	spec, err := reference.Parse(r.host(t) + "/" + r.repo + ":latest")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestRegistryHostTransportReturnsRedirect checks that the transport of the
// registry host client hands 3xx responses back to the caller instead of
// following them internally. fs/remote relies on this to cache the redirected
// location.
func TestRegistryHostTransportReturnsRedirect(t *testing.T) {
	reg := newRedirectingRegistry(t, 16)
	hosts, err := RegistryHostsFromConfig(Config{})(reg.refspec(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/v2/%s/blobs/%s", reg.registry.URL, reg.repo, reg.dgst), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := hosts[0].Client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected transport to return %d, got %d", http.StatusTemporaryRedirect, res.StatusCode)
	}
	if loc := res.Header.Get("Location"); !strings.HasPrefix(loc, reg.storage.URL) {
		t.Fatalf("unexpected Location %q", loc)
	}
	if got := reg.storageHits.Load(); got != 0 {
		t.Fatalf("transport followed the redirect: storage got %d requests", got)
	}
}

// TestRegistryHostClientFollowsRedirect checks that users of the full
// http.Client (containerd's docker resolver, docker.Authorizer) still get
// redirects followed.
func TestRegistryHostClientFollowsRedirect(t *testing.T) {
	reg := newRedirectingRegistry(t, 16)
	hosts, err := RegistryHostsFromConfig(Config{})(reg.refspec(t))
	if err != nil {
		t.Fatal(err)
	}
	res, err := hosts[0].Client.Get(fmt.Sprintf("%s/v2/%s/blobs/%s", reg.registry.URL, reg.repo, reg.dgst))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected client to follow the redirect and get %d, got %d", http.StatusOK, res.StatusCode)
	}
}

// TestRemoteBlobFetchesFromRedirectedLocation resolves a blob with fs/remote
// using the registry hosts and reads several chunks. The registry must be
// contacted only once (to resolve the redirect); all range requests must go
// directly to the storage host.
func TestRemoteBlobFetchesFromRedirectedLocation(t *testing.T) {
	const (
		chunkSize = 1024
		blobSize  = 64 * chunkSize
	)
	reg := newRedirectingRegistry(t, blobSize)
	refspec := reg.refspec(t)

	r := remote.NewResolver(config.BlobConfig{ChunkSize: chunkSize}, nil)
	desc := ocispec.Descriptor{Digest: reg.dgst, Size: int64(blobSize)}
	b, err := r.Resolve(context.Background(), RegistryHostsFromConfig(Config{}), refspec, desc, cache.NewMemoryCache())
	if err != nil {
		t.Fatalf("failed to resolve blob: %v", err)
	}
	defer b.Close()
	if b.Size() != int64(blobSize) {
		t.Fatalf("unexpected blob size %d; want %d", b.Size(), blobSize)
	}

	registryHitsAfterResolve := reg.registryHits.Load()
	if registryHitsAfterResolve != 1 {
		t.Errorf("expected 1 registry request to resolve the blob, got %d", registryHitsAfterResolve)
	}

	// Read every other chunk so that each read needs its own range request.
	const reads = 16
	for i := range reads {
		off := int64(i * 2 * chunkSize)
		p := make([]byte, chunkSize)
		n, err := b.ReadAt(p, off)
		if err != nil {
			t.Fatalf("ReadAt(%d) failed: %v", off, err)
		}
		if !bytes.Equal(p[:n], reg.blob[off:off+int64(n)]) {
			t.Fatalf("unexpected content at offset %d", off)
		}
	}

	if got := reg.registryHits.Load() - registryHitsAfterResolve; got != 0 {
		t.Errorf("range requests went through the registry: %d extra registry requests for %d reads", got, reads)
	}
	if got := reg.storageHits.Load(); got < reads {
		t.Errorf("expected at least %d storage requests, got %d", reads, got)
	}
}
