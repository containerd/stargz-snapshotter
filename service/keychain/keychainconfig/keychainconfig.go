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

package keychainconfig

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/containerd/stargz-snapshotter/service/keychain/cri"
	"github.com/containerd/stargz-snapshotter/service/keychain/dockerconfig"
	"github.com/containerd/stargz-snapshotter/service/keychain/kubeconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type Config struct {
	EnableKubeKeychain bool
	EnableCRIKeychain  bool
	KubeconfigPath     string
}

func ConfigKeychain(ctx context.Context, rpc *grpc.Server, config *Config) ([]resolver.Credential, net.Listener, error) {
	credsFuncs := []resolver.Credential{dockerconfig.NewDockerconfigKeychain(ctx)}
	if config.EnableKubeKeychain {
		var opts []kubeconfig.Option
		if kcp := config.KubeconfigPath; kcp != "" {
			opts = append(opts, kubeconfig.WithKubeconfigPath(kcp))
		}
		credsFuncs = append(credsFuncs, kubeconfig.NewKubeconfigKeychain(ctx, opts...))
	}
	var lis net.Listener
	if config.EnableCRIKeychain {
		bc := newPipeListener()
		lis = bc
		connectCRI := func() (runtime.ImageServiceClient, error) {
			conn, err := grpc.NewClient("passthrough://localhost:0",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
					return bc.dial()
				}),
			)
			if err != nil {
				return nil, err
			}
			return runtime.NewImageServiceClient(conn), nil
		}
		f, criServer := cri.NewCRIKeychain(ctx, connectCRI)
		runtime.RegisterImageServiceServer(rpc, criServer)
		credsFuncs = append(credsFuncs, f)
	}

	return credsFuncs, lis, nil
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		ch:   make(chan net.Conn),
		done: make(chan struct{}),
	}
}

type pipeListener struct {
	ch        chan net.Conn
	done      chan struct{}
	closed    bool
	closeOnce sync.Once
	closedMu  sync.Mutex
}

func (l *pipeListener) dial() (net.Conn, error) {
	if l.isClosed() {
		return nil, fmt.Errorf("closed")
	}
	c1, c2 := net.Pipe()
	select {
	case <-l.done:
		return nil, fmt.Errorf("closed")
	case l.ch <- c1:
	}
	return c2, nil
}

func (l *pipeListener) Accept() (net.Conn, error) {
	if l.isClosed() {
		return nil, fmt.Errorf("closed")
	}
	select {
	case <-l.done:
		return nil, fmt.Errorf("closed")
	case conn := <-l.ch:
		return conn, nil
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() {
		l.closedMu.Lock()
		l.closed = true
		close(l.done)
		l.closedMu.Unlock()
	})
	return nil
}

func (l *pipeListener) isClosed() bool {
	l.closedMu.Lock()
	defer l.closedMu.Unlock()
	return l.closed
}

func (l *pipeListener) Addr() net.Addr {
	return dummyAddr{}
}

type dummyAddr struct{}

func (a dummyAddr) Network() string { return "passthrough" }
func (a dummyAddr) String() string  { return "passthrough://localhost:0" }
