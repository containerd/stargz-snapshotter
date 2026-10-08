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

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	snproxy "github.com/containerd/containerd/v2/core/snapshots/proxy"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/log"

	"github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/fsopts"
	fusemanager "github.com/containerd/stargz-snapshotter/fusemanager"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/keychainconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"github.com/containerd/stargz-snapshotter/service/verifier"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	defaultCRIContainerdAddress   = "/run/containerd/containerd.sock"
	defaultCRIContainerdNamespace = "k8s.io"
)

func init() {
	fusemanager.RegisterConfigFunc(func(cc *fusemanager.ConfigContext) ([]service.Option, error) {
		fsConfig := fsopts.Config{
			EnableIpfs:    cc.Config.IPFS,
			MetadataStore: cc.Config.MetadataStore,
			OpenBoltDB:    cc.OpenBoltDB,
		}
		fsOpts, err := fsopts.ConfigFsOpts(cc.Ctx, cc.RootDir, &fsConfig)
		if err != nil {
			return nil, err
		}
		return []service.Option{service.WithFilesystemOptions(fsOpts...)}, nil
	})

	fusemanager.RegisterConfigFunc(func(cc *fusemanager.ConfigContext) ([]service.Option, error) {
		criContainerdAddress := cc.Config.CRIContainerdAddress
		if criContainerdAddress == "" {
			criContainerdAddress = defaultCRIContainerdAddress
		}
		criContainerdNamespace := cc.Config.CRIContainerdNamespace
		if criContainerdNamespace == "" {
			criContainerdNamespace = defaultCRIContainerdNamespace
		}
		if cc.Config.Config.CRIKeychainConfig.EnableKeychain && cc.Config.Config.ImageServicePath != "" && cc.Config.Config.ImageServicePath != criContainerdAddress {
			return nil, fmt.Errorf("inconsistent global CRI backend and CRI keychain' backend service %q != %q", criContainerdAddress, cc.Config.Config.ImageServicePath)
		}

		criListenPath := cc.Config.CRIListenPath
		if cc.Config.Config.ListenPath != "" {
			if criListenPath != "" && criListenPath != cc.Config.Config.ListenPath {
				return nil, fmt.Errorf("inconsistent global CRI socket and CRI keychain socket address %q != %q", criListenPath, cc.Config.Config.ListenPath)
			}
			criListenPath = cc.Config.Config.ListenPath
		}

		if cc.Config.Config.CRIKeychainConfig.EnableKeychain {
			cc.CRIServer = grpc.NewServer()
		}

		credsFuncs, criListener, err := keychainconfig.ConfigKeychain(context.Background(), cc.CRIServer, &keychainconfig.Config{
			EnableKubeKeychain: cc.Config.Config.KubeconfigKeychainConfig.EnableKeychain,
			EnableCRIKeychain:  cc.Config.Config.CRIKeychainConfig.EnableKeychain,
			KubeconfigPath:     cc.Config.Config.KubeconfigPath,
		})
		if err != nil {
			return nil, err
		}

		if criListenPath != "" {
			if criListenPath == cc.Address {
				return nil, fmt.Errorf("listen path of CRI server must be specified as a separated socket from FUSE manager server")
			}
			if cc.Config.StargzSnapshotterAddress == "" {
				return nil, fmt.Errorf("stargz snapshotter socket location must be specified from containerd-stargz-grpc")
			}
			criVerifier := verifier.NewCRIVerifier(
				context.Background(),
				cc.Config.Config,
				resolver.RegistryHostsFromConfig(resolver.Config(cc.Config.Config.ResolverConfig), credsFuncs...),
				filepath.Join(cc.RootDir, "manager"),
				criContainerdNamespace,
				func() (runtime.RuntimeServiceClient, runtime.ImageServiceClient, *containerd.Client, error) {
					conn, err := newGRPCConn(criContainerdAddress)
					if err != nil {
						return nil, nil, nil, fmt.Errorf("failed to connect to CRI: %w", err)
					}
					ctdclient, err := containerd.New(criContainerdAddress)
					if err != nil {
						return nil, nil, nil, fmt.Errorf("failed to connect to containerd: %w", err)
					}
					return runtime.NewRuntimeServiceClient(conn), runtime.NewImageServiceClient(conn), ctdclient, nil
				},
				func() snapshots.Snapshotter {
					conn, err := newGRPCConn(cc.Config.StargzSnapshotterAddress)
					if err != nil {
						fmt.Println("failed to connect to snapshotter", err)
						return nil
					}
					return snproxy.NewSnapshotter(snapshotsapi.NewSnapshotsClient(conn), "stargz")
				},
			)
			if criListener != nil {
				// CRI keychain connects to this service
				localCRIRPC := grpc.NewServer()
				runtime.RegisterImageServiceServer(localCRIRPC, criVerifier)
				go localCRIRPC.Serve(criListener)
			} else {
				cc.CRIServer = grpc.NewServer()
				runtime.RegisterImageServiceServer(cc.CRIServer, criVerifier)
			}

			// Prepare the directory for the socket
			if err := os.MkdirAll(filepath.Dir(criListenPath), 0700); err != nil {
				return nil, fmt.Errorf("failed to create directory %q: %w", filepath.Dir(criListenPath), err)
			}

			// Try to remove the socket file to avoid EADDRINUSE
			if err := os.RemoveAll(criListenPath); err != nil {
				return nil, fmt.Errorf("failed to remove %q: %w", criListenPath, err)
			}

			// Listen and serve
			l, err := net.Listen("unix", criListenPath)
			if err != nil {
				return nil, fmt.Errorf("error on listen socket %q: %w", criListenPath, err)
			}
			go func() {
				if err := cc.CRIServer.Serve(l); err != nil {
					log.G(cc.Ctx).WithError(err).Errorf("error on serving CRI via socket %q", criListenPath)
				}
			}()
		}
		return []service.Option{service.WithCredsFuncs(credsFuncs...)}, nil
	})
}

func main() {
	fusemanager.Run()
}

func newGRPCConn(addr string) (*grpc.ClientConn, error) {
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
	return grpc.NewClient(dialer.DialAddress(addr), gopts...)
}
