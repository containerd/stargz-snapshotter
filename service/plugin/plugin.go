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
	"errors"

	"github.com/containerd/containerd/platforms"
	ctdplugin "github.com/containerd/containerd/plugin"
	"github.com/containerd/stargz-snapshotter/service"
)

// Config represents configuration for the stargz snapshotter plugin.
type Config struct {
	service.Config

	// RootPath is the directory for the plugin
	RootPath string `toml:"root_path"`
}

func init() {
	ctdplugin.Register(&ctdplugin.Registration{
		Type:   ctdplugin.SnapshotPlugin,
		ID:     "stargz",
		Config: &Config{},
		InitFn: func(ic *ctdplugin.InitContext) (interface{}, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())

			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, errors.New("invalid stargz snapshotter configuration")
			}

			root := ic.Root
			if config.RootPath != "" {
				root = config.RootPath
			}
			ic.Meta.Exports["root"] = root
			return service.NewStargzSnapshotterService(ic.Context, root, &config.Config)
		},
	})
}
