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

package service

import (
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/service/resolver"
)

type Config struct {
	config.Config

	// KubeconfigKeychainConfig is config for kubeconfig-based keychain.
	KubeconfigKeychainConfig `toml:"kubeconfig_keychain"`

	// ResolverConfig is config for resolving registries.
	ResolverConfig `toml:"resolver"`
}

type KubeconfigKeychainConfig struct {
	EnableKeychain bool   `toml:"enable_keychain"`
	KubeconfigPath string `toml:"kubeconfig_path"`
}

type ResolverConfig resolver.Config
