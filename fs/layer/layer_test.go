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
	"context"
	"io"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/containerd/containerd/reference"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/fs/reader"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/task"
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
	tests := []struct {
		name             string
		in               []testutil.TarEntry
		wantNum          int      // number of chunks wanted in the cache
		wants            []string // filenames to compare
		prefetchSize     func(*testing.T, *layer) int64
		prioritizedFiles []string
	}{
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
			sr, dgst, err := testutil.BuildEStargz(tt.in,
				testutil.WithEStargzOptions(
					estargz.WithChunkSize(sampleChunkSize),
					estargz.WithPrioritizedFiles(tt.prioritizedFiles),
				))
			if err != nil {
				t.Fatalf("failed to build eStargz: %v", err)
			}
			blob := newBlob(sr)
			mcache := cache.NewMemoryCache()
			// define telemetry hooks to measure latency metrics inside estargz package
			telemetry := estargz.Telemetry{}
			vr, err := reader.NewReader(sr, mcache, testStateLayerDigest, &telemetry)
			if err != nil {
				t.Fatalf("failed to make stargz reader: %v", err)
			}
			l := newLayer(
				&Resolver{
					prefetchTimeout:       time.Second,
					backgroundTaskManager: task.NewBackgroundTaskManager(10, 5*time.Second),
				},
				ocispec.Descriptor{Digest: testStateLayerDigest},
				&blobRef{blob, func() {}},
				vr,
			)
			if err := l.Verify(dgst); err != nil {
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
			if cLen := len(mcache.(*cache.MemoryCache).Membuf); tt.wantNum != cLen {
				t.Errorf("number of chunks in the cache %d; want %d: %v", cLen, tt.wantNum, err)
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
func (sb *sampleBlob) Refresh(ctx context.Context, hosts source.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) error {
	return nil
}
func (sb *sampleBlob) Close() error { return nil }

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
