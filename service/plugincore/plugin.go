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

package plugincore

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	ctdplugins "github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/keychainconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"github.com/containerd/stargz-snapshotter/service/verifier"
	"github.com/containerd/stargz-snapshotter/util/criconn"
	grpc "google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	defaultCRIContainerdNamespace string = "k8s.io"
)

// Config represents configuration for the stargz snapshotter plugin.
type Config struct {
	service.Config

	// RootPath is the directory for the plugin
	RootPath string `toml:"root_path"`

	// CRIKeychainImageServicePath is the path to expose CRI service wrapped by stargz-snapshotter
	CRIKeychainImageServicePath string `toml:"cri_keychain_image_service_path"`

	// CRIContainerdNamespace is the containerd namespace configured for CRI. Default is "k8s.io"
	CRIContainerdNamespace string `toml:"cri_containerd_namespace"`

	// Registry is CRI-plugin-compatible registry configuration
	Registry resolver.Registry `toml:"registry"`
}

func RegisterPlugin() {
	registry.Register(&plugin.Registration{
		Type:   ctdplugins.SnapshotPlugin,
		ID:     "stargz",
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())
			ctx := ic.Context

			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, errors.New("invalid stargz snapshotter configuration")
			}

			root := ic.Properties[ctdplugins.PropertyRootDir]
			if config.RootPath != "" {
				root = config.RootPath
			}
			ic.Meta.Exports["root"] = root

			// Create a gRPC server
			rpc := grpc.NewServer()

			// Configure keychain
			credsFuncs, criListener, err := keychainconfig.ConfigKeychain(ctx, rpc, &keychainconfig.Config{
				EnableKubeKeychain: config.KubeconfigKeychainConfig.EnableKeychain,
				EnableCRIKeychain:  config.CRIKeychainConfig.EnableKeychain,
				KubeconfigPath:     config.KubeconfigPath,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to configure keychain")
			}

			var snService snapshots.Snapshotter
			var snServiceMu sync.Mutex
			if addr := config.CRIKeychainImageServicePath; addr != "" {
				ctdAddr := ic.Properties[ctdplugins.PropertyGRPCAddress]
				if cp := config.ImageServicePath; cp != "" {
					ctdAddr = cp
				}
				if ctdAddr == "" {
					return nil, errors.New("backend CRI service address is not specified")
				}

				ns := defaultCRIContainerdNamespace
				if config.CRIContainerdNamespace != "" {
					ns = config.CRIContainerdNamespace
				}
				criVerifier := verifier.NewCRIVerifier(
					ctx,
					config.Config,
					resolver.RegistryHostsFromCRIConfig(ctx, config.Registry, credsFuncs...),
					filepath.Join(root, "verifier"),
					ns,
					func() (runtime.RuntimeServiceClient, runtime.ImageServiceClient, *containerd.Client, error) {
						conn, err := criconn.NewCRIConn(ctdAddr)
						if err != nil {
							return nil, nil, nil, fmt.Errorf("failed to connect to CRI: %w", err)
						}
						ctdclient, err := containerd.New(ctdAddr)
						if err != nil {
							return nil, nil, nil, fmt.Errorf("failed to connect to containerd: %w", err)
						}
						return runtime.NewRuntimeServiceClient(conn), runtime.NewImageServiceClient(conn), ctdclient, nil
					},
					func() snapshots.Snapshotter {
						snServiceMu.Lock()
						defer snServiceMu.Unlock()
						return snService
					},
				)
				if criListener != nil {
					localCRIRPC := grpc.NewServer()
					runtime.RegisterImageServiceServer(localCRIRPC, criVerifier)
					go localCRIRPC.Serve(criListener)
				} else {
					runtime.RegisterImageServiceServer(rpc, criVerifier)
				}

				// Prepare the directory for the socket
				if err := os.MkdirAll(filepath.Dir(addr), 0700); err != nil {
					return nil, fmt.Errorf("failed to create directory %q: %w", filepath.Dir(addr), err)
				}
				// Try to remove the socket file to avoid EADDRINUSE
				if err := os.RemoveAll(addr); err != nil {
					return nil, fmt.Errorf("failed to remove %q: %w", addr, err)
				}
				// Listen and serve
				l, err := net.Listen("unix", addr)
				if err != nil {
					return nil, fmt.Errorf("error on listen socket %q: %w", addr, err)
				}
				go func() {
					if err := rpc.Serve(l); err != nil {
						log.G(ctx).WithError(err).Warnf("error on serving via socket %q", addr)
					}
				}()
			}

			// TODO(ktock): print warn if old configuration is specified.
			// TODO(ktock): should we respect old configuration?
			snServiceMu.Lock()
			snService, err = service.NewStargzSnapshotterService(ctx, root, &config.Config,
				service.WithCustomRegistryHosts(resolver.RegistryHostsFromCRIConfig(ctx, config.Registry, credsFuncs...)))
			snServiceMu.Unlock()
			return snService, err
		},
	})
}
