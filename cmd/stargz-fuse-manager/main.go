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
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/containerd/log"

	"github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/fsopts"
	fusemanager "github.com/containerd/stargz-snapshotter/fusemanager"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/keychainconfig"
	"google.golang.org/grpc"
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
		keyChainConfig := keychainconfig.Config{
			EnableKubeKeychain:         cc.Config.Config.KubeconfigKeychainConfig.EnableKeychain,
			EnableCRIKeychain:          cc.Config.Config.CRIKeychainConfig.EnableKeychain,
			KubeconfigPath:             cc.Config.Config.KubeconfigPath,
			DefaultImageServiceAddress: cc.Config.DefaultImageServiceAddress,
			ImageServicePath:           cc.Config.Config.ImageServicePath,
		}
		if cc.Config.Config.CRIKeychainConfig.EnableKeychain && cc.Config.Config.ListenPath == "" || cc.Config.Config.ListenPath == cc.Address {
			return nil, fmt.Errorf("listen path of CRI server must be specified as a separated socket from FUSE manager server")
		}
		// For CRI keychain, if listening path is different from stargz-snapshotter's socket, prepare for the dedicated grpc server and the socket.
		serveCRISocket := cc.Config.Config.CRIKeychainConfig.EnableKeychain && cc.Config.Config.ListenPath != "" && cc.Config.Config.ListenPath != cc.Address
		if serveCRISocket {
			cc.CRIServer = grpc.NewServer()
		}
		credsFuncs, err := keychainconfig.ConfigKeychain(cc.Ctx, cc.CRIServer, &keyChainConfig)
		if err != nil {
			return nil, err
		}
		if serveCRISocket {
			addr := cc.Config.Config.ListenPath
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
				if err := cc.CRIServer.Serve(l); err != nil {
					log.G(cc.Ctx).WithError(err).Errorf("error on serving CRI via socket %q", addr)
				}
			}()
		}
		return []service.Option{service.WithCredsFuncs(credsFuncs...)}, nil
	})
}

func main() {
	fusemanager.Run()
}
