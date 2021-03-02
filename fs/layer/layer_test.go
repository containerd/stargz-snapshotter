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

package layer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/fs/reader"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	sampleChunkSize = 3
	sampleData1     = "0123456789"
	sampleData2     = "abcdefghij"
)

var testStateLayerDigest = digest.FromString("dummy")

// Tests prefetch method of each stargz file.
func TestPrefetch(t *testing.T) {
	defaultPrefetchSize := int64(10000)
	landmarkPosition := func(t *testing.T, l *layer) int64 {
		if l.r == nil {
			t.Fatalf("layer hasn't been verified yet")
		}
		if e, ok := l.r.Lookup(estargz.PrefetchLandmark); ok {
			return e.Offset
		}
		return defaultPrefetchSize
	}
	defaultPrefetchPosition := func(t *testing.T, l *layer) int64 {
		return l.Info().Size
	}
	tests := []struct {
		name             string
		in               []testutil.TarEntry
		wantNum          int      // number of chunks wanted in the cache
		wants            []string // filenames to compare
		prefetchSize     func(*testing.T, *layer) int64
		prioritizedFiles []string
		stargz           bool
	}{
		{
			name: "default_prefetch",
			in: []testutil.TarEntry{
				testutil.File("foo.txt", sampleData1),
			},
			wantNum:          chunkNum(sampleData1),
			wants:            []string{"foo.txt"},
			prefetchSize:     defaultPrefetchPosition,
			prioritizedFiles: nil,
			stargz:           true,
		},
		{
			name: "no_prefetch",
			in: []testutil.TarEntry{
				testutil.File("foo.txt", sampleData1),
			},
			wantNum:          0,
			prioritizedFiles: nil,
		},
		{
			name: "prefetch",
			in: []testutil.TarEntry{
				testutil.File("foo.txt", sampleData1),
				testutil.File("bar.txt", sampleData2),
			},
			wantNum:          chunkNum(sampleData1),
			wants:            []string{"foo.txt"},
			prefetchSize:     landmarkPosition,
			prioritizedFiles: []string{"foo.txt"},
		},
		{
			name: "with_dir",
			in: []testutil.TarEntry{
				testutil.Dir("foo/"),
				testutil.File("foo/bar.txt", sampleData1),
				testutil.Dir("buz/"),
				testutil.File("buz/buzbuz.txt", sampleData2),
			},
			wantNum:          chunkNum(sampleData1),
			wants:            []string{"foo/bar.txt"},
			prefetchSize:     landmarkPosition,
			prioritizedFiles: []string{"foo/", "foo/bar.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr, dgst := buildStargz(t, tt.in,
				chunkSizeInfo(sampleChunkSize),
				prioritizedFilesInfo(tt.prioritizedFiles),
				stargzOnlyInfo(tt.stargz))
			blob := newBlob(sr)
			cache := &testCache{membuf: map[string]string{}, t: t}
			vr, _, err := reader.NewReader(sr, cache)
			if err != nil {
				t.Fatalf("failed to make stargz reader: %v", err)
			}
			l := newLayer(
				&Resolver{
					prefetchTimeout: time.Second,
				},
				ocispec.Descriptor{Digest: testStateLayerDigest},
				&blobRef{blob, func() {}},
				vr,
				nil,
			)
			if tt.stargz {
				l.SkipVerify()
			} else if err := l.Verify(dgst); err != nil {
				t.Errorf("failed to verify reader: %v", err)
				return
			}
			prefetchSize := int64(0)
			if tt.prefetchSize != nil {
				prefetchSize = tt.prefetchSize(t, l)
			}
			if err := l.Prefetch(defaultPrefetchSize); err != nil {
				t.Errorf("failed to prefetch: %v", err)
				return
			}
			if blob.calledPrefetchOffset != 0 {
				t.Errorf("invalid prefetch offset %d; want %d",
					blob.calledPrefetchOffset, 0)
			}
			if blob.calledPrefetchSize != prefetchSize {
				t.Errorf("invalid prefetch size %d; want %d",
					blob.calledPrefetchSize, prefetchSize)
			}
			if tt.wantNum != len(cache.membuf) {
				t.Errorf("number of chunks in the cache %d; want %d: %v", len(cache.membuf), tt.wantNum, err)
				return
			}

			lr := l.r
			if lr == nil {
				t.Fatalf("failed to get reader from layer: %v", err)
			}
			for _, file := range tt.wants {
				e, ok := lr.Lookup(file)
				if !ok {
					t.Fatalf("failed to lookup %q", file)
				}
				wantFile, err := lr.OpenFile(file)
				if err != nil {
					t.Fatalf("failed to open file %q", file)
				}
				blob.readCalled = false
				if _, err := io.Copy(ioutil.Discard, io.NewSectionReader(wantFile, 0, e.Size)); err != nil {
					t.Fatalf("failed to read file %q", file)
				}
				if blob.readCalled {
					t.Errorf("chunks of file %q aren't cached", file)
					return
				}
			}
		})
	}
}

func chunkNum(data string) int {
	return (len(data)-1)/sampleChunkSize + 1
}

func newBlob(sr *io.SectionReader) *sampleBlob {
	return &sampleBlob{
		r: sr,
	}
}

type sampleBlob struct {
	r                    *io.SectionReader
	readCalled           bool
	calledPrefetchOffset int64
	calledPrefetchSize   int64
}

func (sb *sampleBlob) Close() error                                          { return nil }
func (sb *sampleBlob) Authn(tr http.RoundTripper) (http.RoundTripper, error) { return nil, nil }
func (sb *sampleBlob) Check() error                                          { return nil }
func (sb *sampleBlob) Size() int64                                           { return sb.r.Size() }
func (sb *sampleBlob) FetchedSize() int64                                    { return 0 }
func (sb *sampleBlob) ReadAt(p []byte, offset int64, opts ...remote.Option) (int, error) {
	sb.readCalled = true
	return sb.r.ReadAt(p, offset)
}
func (sb *sampleBlob) Cache(offset int64, size int64, option ...remote.Option) error {
	sb.calledPrefetchOffset = offset
	sb.calledPrefetchSize = size
	return nil
}
func (sb *sampleBlob) Refresh(ctx context.Context, hosts docker.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error {
	return nil
}

type testCache struct {
	membuf map[string]string
	t      *testing.T
	mu     sync.Mutex
}

func (tc *testCache) FetchAt(key string, offset int64, p []byte, opts ...cache.Option) (int, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	cache, ok := tc.membuf[key]
	if !ok {
		return 0, fmt.Errorf("Missed cache: %q", key)
	}
	return copy(p, cache[offset:]), nil
}

func (tc *testCache) Add(key string, p []byte, opts ...cache.Option) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.membuf[key] = string(p)
	tc.t.Logf("  cached [%s...]: %q", key[:8], string(p))
}
func (tc *testCache) Close() error {
	return nil
}

func TestWaiter(t *testing.T) {
	var (
		w         = newWaiter()
		waitTime  = time.Second
		startTime = time.Now()
		doneTime  time.Time
		done      = make(chan struct{})
	)

	go func() {
		defer close(done)
		if err := w.wait(10 * time.Second); err != nil {
			t.Errorf("failed to wait: %v", err)
			return
		}
		doneTime = time.Now()
	}()

	time.Sleep(waitTime)
	w.done()
	<-done

	if doneTime.Sub(startTime) < waitTime {
		t.Errorf("wait time is too short: %v; want %v", doneTime.Sub(startTime), waitTime)
	}
}

type chunkSizeInfo int
type prioritizedFilesInfo []string
type stargzOnlyInfo bool

func buildStargz(t *testing.T, ents []testutil.TarEntry, opts ...interface{}) (*io.SectionReader, digest.Digest) {
	var chunkSize chunkSizeInfo
	var prioritizedFiles prioritizedFilesInfo
	var stargzOnly bool
	for _, opt := range opts {
		if v, ok := opt.(chunkSizeInfo); ok {
			chunkSize = v
		} else if v, ok := opt.(prioritizedFilesInfo); ok {
			prioritizedFiles = v
		} else if v, ok := opt.(stargzOnlyInfo); ok {
			stargzOnly = bool(v)
		} else {
			t.Fatalf("unsupported opt")
		}
	}

	tarBuf := new(bytes.Buffer)
	if _, err := io.Copy(tarBuf, testutil.BuildTar(ents)); err != nil {
		t.Fatalf("failed to build tar: %v", err)
	}
	tarData := tarBuf.Bytes()

	if stargzOnly {
		stargzBuf := new(bytes.Buffer)
		w := estargz.NewWriter(stargzBuf)
		if chunkSize > 0 {
			w.ChunkSize = int(chunkSize)
		}
		if err := w.AppendTar(bytes.NewReader(tarData)); err != nil {
			t.Fatalf("failed to append tar file to stargz: %q", err)
		}
		if _, err := w.Close(); err != nil {
			t.Fatalf("failed to close stargz writer: %q", err)
		}
		stargzData := stargzBuf.Bytes()
		return io.NewSectionReader(bytes.NewReader(stargzData), 0, int64(len(stargzData))), ""
	}
	rc, err := estargz.Build(
		io.NewSectionReader(bytes.NewReader(tarData), 0, int64(len(tarData))),
		estargz.WithPrioritizedFiles([]string(prioritizedFiles)),
		estargz.WithChunkSize(int(chunkSize)),
	)
	if err != nil {
		t.Fatalf("failed to build verifiable stargz: %v", err)
	}
	defer rc.Close()
	vsb := new(bytes.Buffer)
	if _, err := io.Copy(vsb, rc); err != nil {
		t.Fatalf("failed to copy built stargz blob: %v", err)
	}
	vsbb := vsb.Bytes()

	return io.NewSectionReader(bytes.NewReader(vsbb), 0, int64(len(vsbb))), rc.TOCDigest()
}
