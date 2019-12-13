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
	"flag"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/contrib/snapshotservice"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/plugin"
	srvconfig "github.com/containerd/containerd/services/server/config"
	snplugin "github.com/containerd/containerd/snapshots"
	fsplugin "github.com/ktock/remote-snapshotter/filesystems"
	_ "github.com/ktock/remote-snapshotter/snapshot"
	"github.com/sirupsen/logrus"
)

const (
	defaultStateDir   = "/run/rsnapshotd"
	defaultRootDir    = "/var/lib/rsnapshotd"
	defaultConfigPath = "/etc/rsnapshotd/config.toml"
	defaultAddress    = "/run/rsnapshotd/rsnapshotd.sock"
	defaultPluginsDir = "/opt/rsnapshotd/plugins"
	defaultLogLevel   = logrus.InfoLevel
)

var (
	configPath = flag.String("config", defaultConfigPath, "path to the configuration file")
	logLevel   = flag.String("log-level", defaultLogLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	address    = flag.String("address", defaultAddress, "address for remote-snapshotter's GRPC server")
	rootDir    = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
	stateDir   = flag.String("state", defaultStateDir, "path to the state directory for this snapshotter")
	pluginsDir = flag.String("plugins", defaultPluginsDir, "path to the plugins directory")
)

func main() {
	flag.Parse()
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		fmt.Print(err)
		os.Exit(1)
	}
	logrus.SetLevel(lvl)

	ctx := log.WithLogger(context.Background(), log.L)
	config := &srvconfig.Config{
		Version: 1,
		Root:    *rootDir,
		State:   *stateDir,
	}
	if *configPath != "" {
		if err := srvconfig.LoadConfig(*configPath, config); err != nil {
			fmt.Printf("failed to load config file %q, %q", *configPath, err)
			os.Exit(1)
		}
	}
	var rs snplugin.Snapshotter
	if *pluginsDir != "" {
		if err := plugin.Load(*pluginsDir); err != nil {
			fmt.Printf("failed to load plugin from %q: %q", *pluginsDir, err)
			os.Exit(1)
		}
	}
	plugins := plugin.Graph(func(r *plugin.Registration) bool {
		if r.Type == fsplugin.RemoteFileSystemPlugin || r.ID == "remote" {
			return false
		}
		return true
	})
	initialized := plugin.NewPluginSet()
	for _, p := range plugins {
		initContext := plugin.NewContext(
			ctx,
			p,
			initialized,
			config.Root,
			config.State,
		)
		if p.Config != nil {
			pc, err := config.Decode(p)
			if err != nil {
				fmt.Printf("failed to parse config file: %q", err)
				os.Exit(1)
			}
			initContext.Config = pc
		}
		result := p.Init(initContext)
		if err := initialized.Add(result); err != nil {
			fmt.Printf("failed to add plugin %q result to plugin set: %q", p.ID, err)
			os.Exit(1)
		}
		i, err := result.Instance()
		if err != nil {
			fmt.Printf("failed to load plugin %q: %q", p.ID, err)
			os.Exit(1)
		}
		if sn, ok := i.(snplugin.Snapshotter); ok {
			rs = sn
			log.G(ctx).Infof("Registered snapshotter plugin %s", p.ID)
		}
	}

	if rs == nil {
		fmt.Println("failed to register snapshotter")
		os.Exit(1)
	}

	// Create a gRPC server
	rpc := grpc.NewServer()

	// Convert the snapshotter to a gRPC service,
	service := snapshotservice.FromSnapshotter(rs)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, service)

	// Listen and serve
	l, err := net.Listen("unix", *address)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
	if err := rpc.Serve(l); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
