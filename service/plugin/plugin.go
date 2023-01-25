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

package plugin

import (
	"context"
	"time"

	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/pkg/dialer"
	grpc "google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	runtime_alpha "github.com/containerd/containerd/third_party/k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"github.com/containerd/stargz-snapshotter/service/keychain/crialpha"
	"github.com/containerd/stargz-snapshotter/service/plugincore"
	"github.com/containerd/stargz-snapshotter/service/resolver"
)

func init() {
	plugincore.RegisterPlugin(registerCRIAlphaServer)
}

func registerCRIAlphaServer(ctx context.Context, criAddr string, rpc *grpc.Server) resolver.Credential {
	connectAlphaCRI := func() (runtime_alpha.ImageServiceClient, error) {
		conn, err := newCRIConn(criAddr)
		if err != nil {
			return nil, err
		}
		return runtime_alpha.NewImageServiceClient(conn), nil
	}
	criAlphaCreds, criAlphaServer := crialpha.NewCRIAlphaKeychain(ctx, connectAlphaCRI)
	runtime_alpha.RegisterImageServiceServer(rpc, criAlphaServer)
	return criAlphaCreds
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
	return grpc.Dial(dialer.DialAddress(criAddr), gopts...)
}
