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
	"time"

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/stargz-snapshotter/service/keychain/cri"
	"github.com/containerd/stargz-snapshotter/service/keychain/dockerconfig"
	"github.com/containerd/stargz-snapshotter/service/keychain/kubeconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type Config struct {
	EnableKubeKeychain         bool
	EnableCRIKeychain          bool
	KubeconfigPath             string
	DefaultImageServiceAddress string
	ImageServicePath           string
}

func ConfigKeychain(ctx context.Context, rpc *grpc.Server, config *Config) ([]resolver.Credential, error) {
	credsFuncs := []resolver.Credential{dockerconfig.NewDockerconfigKeychain(ctx)}
	if config.EnableKubeKeychain {
		var opts []kubeconfig.Option
		if kcp := config.KubeconfigPath; kcp != "" {
			opts = append(opts, kubeconfig.WithKubeconfigPath(kcp))
		}
		credsFuncs = append(credsFuncs, kubeconfig.NewKubeconfigKeychain(ctx, opts...))
	}
	if config.EnableCRIKeychain {
		// connects to the backend CRI service (defaults to containerd socket)
		criAddr := config.DefaultImageServiceAddress
		if cp := config.ImageServicePath; cp != "" {
			criAddr = cp
		}
		connectCRI := func() (runtime.ImageServiceClient, error) {
			conn, err := newCRIConn(criAddr)
			if err != nil {
				return nil, err
			}
			return runtime.NewImageServiceClient(conn), nil
		}
		f, criServer := cri.NewCRIKeychain(ctx, connectCRI)
		runtime.RegisterImageServiceServer(rpc, criServer)
		credsFuncs = append(credsFuncs, f)
	}

	return credsFuncs, nil
}

func newCRIConn(criAddr string) (*grpc.ClientConn, error) {
	// TODO: make gRPC options configurable from config.toml
	backoffConfig := backoff.DefaultConfig
	backoffConfig.MaxDelay = 3 * time.Second
	connParams := grpc.ConnectParams{
		Backoff: backoffConfig,
	}
	gopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connParams),
		grpc.WithContextDialer(dialer.ContextDialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
	}
	return grpc.NewClient(dialer.DialAddress(criAddr), gopts...)
}
