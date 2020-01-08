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
	"net"
	"os"
	"os/signal"
	"path/filepath"

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
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	logrus.SetLevel(lvl)
	logrus.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
		FullTimestamp:   true,
	})

	ctx := log.WithLogger(context.Background(), log.L)
	config := &srvconfig.Config{
		Version: 1,
		Root:    *rootDir,
		State:   *stateDir,
	}
	if *configPath != "" {
		if err := srvconfig.LoadConfig(*configPath, config); err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
		}
	}
	var rs snplugin.Snapshotter
	if *pluginsDir != "" {
		if err := plugin.Load(*pluginsDir); err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to load plugin from %q", *pluginsDir)
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
				log.G(ctx).WithError(err).Fatal("failed to parse config file")
			}
			initContext.Config = pc
		}
		result := p.Init(initContext)
		if err := initialized.Add(result); err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to add plugin %q to plugin set", p.ID)
		}
		i, err := result.Instance()
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to load plugin %q", p.ID)
		}
		if sn, ok := i.(snplugin.Snapshotter); ok {
			log.G(ctx).Infof("Registering snapshotter plugin %q...", p.ID)
			rs = sn
		}
	}

	if rs == nil {
		log.G(ctx).Fatalf("failed to register snapshotter")
	}

	defer func() {
		log.G(ctx).Debug("Closing the snapshotter")
		rs.Close()
		log.G(ctx).Info("Exiting")
	}()

	// Create a gRPC server
	rpc := grpc.NewServer()

	// Convert the snapshotter to a gRPC service,
	service := snapshotservice.FromSnapshotter(rs)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, service)

	// Prepare the directory for the socket
	if err := os.MkdirAll(filepath.Dir(*address), 0700); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to create directory %q", filepath.Dir(*address))
	}

	// Try to remove the socket file to avoid EADDRINUSE
	if err := os.RemoveAll(*address); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to remove %q", *address)
	}

	// Listen and serve
	l, err := net.Listen("unix", *address)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("error on listen socket %q", *address)
	}
	go func() {
		if err := rpc.Serve(l); err != nil {
			log.G(ctx).WithError(err).Fatalf("error on serving via socket %q", *address)
		}
	}()
	waitForSIGINT()
	log.G(ctx).Info("Got SIGINT")
}

func waitForSIGINT() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}
