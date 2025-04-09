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
	"github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/fsopts"
	fusemanager "github.com/containerd/stargz-snapshotter/fusemanager"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/keychainconfig"
)

func init() {
	fusemanager.RegisterConfigFunc(func(cc *fusemanager.ConfigContext) ([]service.Option, error) {
		fsConfig := fsopts.Config{
			EnableIpfs:    cc.Config.IPFS,
			MetadataStore: cc.Config.MetadataStore,
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
		credsFuncs, err := keychainconfig.ConfigKeychain(cc.Ctx, cc.Server, &keyChainConfig)
		if err != nil {
			return nil, err
		}
		return []service.Option{service.WithCredsFuncs(credsFuncs...)}, nil
	})
}

func main() {
	fusemanager.Run()
}
