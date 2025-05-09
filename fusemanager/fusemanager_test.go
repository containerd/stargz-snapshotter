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

package fusemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/containerd/stargz-snapshotter/fusemanager/api"
	"github.com/containerd/stargz-snapshotter/service"
	"google.golang.org/grpc"
)

// mockFileSystem implements snapshot.FileSystem for testing
type mockFileSystem struct {
	t             *testing.T
	mountErr      error
	checkErr      error
	unmountErr    error
	mountPoints   map[string]bool
	checkCalled   bool
	mountCalled   bool
	unmountCalled bool
}

func newMockFileSystem(t *testing.T) *mockFileSystem {
	return &mockFileSystem{
		t:           t,
		mountPoints: make(map[string]bool),
	}
}

func (fs *mockFileSystem) Mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	fs.mountCalled = true
	if fs.mountErr != nil {
		return fs.mountErr
	}
	fs.mountPoints[mountpoint] = true
	return nil
}

func (fs *mockFileSystem) Check(ctx context.Context, mountpoint string, labels map[string]string) error {
	fs.checkCalled = true
	if fs.checkErr != nil {
		return fs.checkErr
	}
	if _, ok := fs.mountPoints[mountpoint]; !ok {
		return fmt.Errorf("mountpoint %s not found", mountpoint)
	}
	return nil
}

func (fs *mockFileSystem) Unmount(ctx context.Context, mountpoint string) error {
	fs.unmountCalled = true
	if fs.unmountErr != nil {
		return fs.unmountErr
	}
	delete(fs.mountPoints, mountpoint)
	return nil
}

// mockServer embeds Server struct and overrides Init method
type mockServer struct {
	*Server
	initCalled bool
	initErr    error
}

func newMockServer(ctx context.Context, listener net.Listener, server *grpc.Server, fuseStoreAddr, serverAddr string) (*mockServer, error) {
	s, err := NewFuseManager(ctx, listener, server, fuseStoreAddr, serverAddr)
	if err != nil {
		return nil, err
	}
	return &mockServer{Server: s}, nil
}

// Init overrides Server.Init to avoid actual initialization
func (s *mockServer) Init(ctx context.Context, req *pb.InitRequest) (*pb.Response, error) {
	s.initCalled = true
	if s.initErr != nil {
		return nil, s.initErr
	}

	// Set only required fields
	s.root = req.Root
	config := &Config{}
	if err := json.Unmarshal(req.Config, config); err != nil {
		return nil, err
	}
	s.config = config
	s.status = FuseManagerReady

	return &pb.Response{}, nil
}

func TestFuseManager(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fusemanager-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	socketPath := filepath.Join(tmpDir, "test.sock")
	fuseStorePath := filepath.Join(tmpDir, "fusestore.db")
	fuseManagerSocketPath := filepath.Join(tmpDir, "test-fusemanager.sock")

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	// Create server with mock
	grpcServer := grpc.NewServer()
	mockFs := newMockFileSystem(t)
	fm, err := newMockServer(context.Background(), l, grpcServer, fuseStorePath, fuseManagerSocketPath)
	if err != nil {
		t.Fatalf("failed to create fuse manager: %v", err)
	}
	defer fm.Close(context.Background())

	pb.RegisterStargzFuseManagerServiceServer(grpcServer, fm)

	// Set mock filesystem
	fm.curFs = mockFs

	go grpcServer.Serve(l)
	defer grpcServer.Stop()

	// Test cases to verify Init, Mount, Check and Unmount operations
	testCases := []struct {
		name       string
		mountpoint string
		labels     map[string]string
		initErr    error
		mountErr   error
		checkErr   error
		unmountErr error
		wantErr    bool
	}{
		{
			name:       "successful init and mount",
			mountpoint: filepath.Join(tmpDir, "mount1"),
			labels:     map[string]string{"key": "value"},
		},
		{
			name:       "init error",
			mountpoint: filepath.Join(tmpDir, "mount2"),
			initErr:    fmt.Errorf("init error"),
			wantErr:    true,
		},
		{
			name:       "mount error",
			mountpoint: filepath.Join(tmpDir, "mount3"),
			mountErr:   fmt.Errorf("mount error"),
			wantErr:    true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockFs.mountErr = tc.mountErr
			mockFs.checkErr = tc.checkErr
			mockFs.unmountErr = tc.unmountErr
			mockFs.mountCalled = false
			mockFs.checkCalled = false
			mockFs.unmountCalled = false
			fm.initErr = tc.initErr
			fm.initCalled = false

			config := &Config{
				Config: service.Config{},
			}
			client, err := NewManagerClient(context.Background(), tmpDir, socketPath, config)
			if err != nil {
				if !tc.wantErr {
					t.Fatalf("failed to create client: %v", err)
				}
				return
			}

			if !fm.initCalled {
				t.Error("Init() was not called")
			}

			if !tc.wantErr {
				// Test Mount
				err = client.Mount(context.Background(), tc.mountpoint, tc.labels)
				if err != nil {
					t.Errorf("Mount() error = %v", err)
				}
				if !mockFs.mountCalled {
					t.Error("Mount() was not called on filesystem")
				}

				// Test Check
				err = client.Check(context.Background(), tc.mountpoint, tc.labels)
				if err != nil {
					t.Errorf("Check() error = %v", err)
				}
				if !mockFs.checkCalled {
					t.Error("Check() was not called on filesystem")
				}

				// Test Unmount
				err = client.Unmount(context.Background(), tc.mountpoint)
				if err != nil {
					t.Errorf("Unmount() error = %v", err)
				}
				if !mockFs.unmountCalled {
					t.Error("Unmount() was not called on filesystem")
				}
			}
		})
	}
}
