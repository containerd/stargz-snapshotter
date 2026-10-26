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

package cri

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/util/namedmutex"
	"github.com/opencontainers/go-digest"
	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestLockDiffIDs(t *testing.T) {
	a := digest.FromString("a")
	b := digest.FromString("b")
	c := digest.FromString("c")

	tests := []struct {
		name    string
		diffIDs []digest.Digest
	}{
		{name: "unique", diffIDs: []digest.Digest{a, b, c}},
		{name: "duplicated", diffIDs: []digest.Digest{a, b, a}},
		{name: "duplicated adjacent", diffIDs: []digest.Digest{c, c, b}},
		{name: "all same", diffIDs: []digest.Digest{a, a, a}},
		{name: "empty", diffIDs: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &instrumentedService{verifyLock: new(namedmutex.NamedMutex)}

			unlock := mustReturn(t, func() func() { return in.lockDiffIDs(tt.diffIDs) })

			for _, d := range tt.diffIDs {
				second := make(chan func())
				go func() { second <- in.lockDiffIDs([]digest.Digest{d}) }()
				select {
				case u := <-second:
					u()
					t.Fatalf("lock of %v must be held until unlock", d)
				case <-time.After(100 * time.Millisecond):
				}
				unlock()
				u := mustReturn(t, func() func() { return <-second })
				u()
				unlock = mustReturn(t, func() func() { return in.lockDiffIDs(tt.diffIDs) })
			}
			unlock()

			mustReturn(t, func() func() { return in.lockDiffIDs(tt.diffIDs) })()
		})
	}
}

func mustReturn(t *testing.T, f func() func()) func() {
	t.Helper()
	ch := make(chan func(), 1)
	go func() { ch <- f() }()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock detected")
		return nil
	}
}

type fakeRuntimeService struct {
	runtime.RuntimeServiceClient
}

type fakeImageService struct {
	runtime.ImageServiceClient
}

func (fakeImageService) ImageFsInfo(context.Context, *runtime.ImageFsInfoRequest, ...grpc.CallOption) (*runtime.ImageFsInfoResponse, error) {
	return &runtime.ImageFsInfoResponse{}, nil
}

type fakeSnapshotter struct {
	snapshots.Snapshotter
}

func TestRequestBeforeInitialized(t *testing.T) {
	var snapshotterCalls atomic.Int32
	server := NewCRIVerifier(
		context.Background(),
		service.Config{},
		nil,
		t.TempDir(),
		"k8s.io",
		func() (runtime.RuntimeServiceClient, runtime.ImageServiceClient, *containerd.Client, error) {
			return fakeRuntimeService{}, fakeImageService{}, nil, nil
		},
		func() snapshots.Snapshotter {
			if snapshotterCalls.Add(1) == 1 {
				return nil
			}
			return fakeSnapshotter{}
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := server.ImageFsInfo(ctx, &runtime.ImageFsInfoRequest{}); err != nil {
		t.Fatalf("request issued before initialization must succeed after initialization: %v", err)
	}
	if n := snapshotterCalls.Load(); n < 2 {
		t.Fatalf("request must wait for initialization; snapshotter getter called %d times", n)
	}
}

func TestRequestCanceledBeforeInitialized(t *testing.T) {
	in := &instrumentedService{ready: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := in.ImageFsInfo(ctx, &runtime.ImageFsInfoRequest{}); err == nil {
		t.Fatal("request must fail when the context is done before initialization")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("request must return after the context is done: %v", ctx.Err())
	}
}
