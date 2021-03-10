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
	golog "log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/BurntSushi/toml"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/contrib/snapshotservice"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/version"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	metrics "github.com/docker/go-metrics"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

const (
	defaultAddress    = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
	defaultConfigPath = "/etc/containerd-stargz-grpc/config.toml"
	defaultLogLevel   = logrus.InfoLevel
	defaultRootDir    = "/var/lib/containerd-stargz-grpc"
)

var (
	address      = flag.String("address", defaultAddress, "address for the snapshotter's GRPC server")
	configPath   = flag.String("config", defaultConfigPath, "path to the configuration file")
	logLevel     = flag.String("log-level", defaultLogLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	rootDir      = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
	printVersion = flag.Bool("version", false, "print the version")
)

type snapshotterConfig struct {
	service.Config

	// MetricsAddress is address for the metrics API
	MetricsAddress string `toml:"metrics_address"`
}

func main() {
	flag.Parse()
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	if *printVersion {
		fmt.Println("containerd-stargz-grpc", version.Version, version.Revision)
		return
	}
	logrus.SetLevel(lvl)
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
	})

	var (
		ctx    = log.WithLogger(context.Background(), log.L)
		config snapshotterConfig
	)
	// Streams log of standard lib (go-fuse uses this) into debug log
	// Snapshotter should use "github.com/containerd/containerd/log" otherwize
	// logs are always printed as "debug" mode.
	golog.SetOutput(log.G(ctx).WriterLevel(logrus.DebugLevel))

	// Get configuration from specified file
	if _, err := toml.DecodeFile(*configPath, &config); err != nil && !(os.IsNotExist(err) && *configPath == defaultConfigPath) {
		log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
	}

	if err := service.Supported(*rootDir); err != nil {
		log.G(ctx).WithError(err).Fatalf("snapshotter is not supported")
	}

	rs, err := service.NewStargzSnapshotterService(ctx, *rootDir, &config.Config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to configure snapshotter")
	}

	cleanup, err := serve(ctx, *address, rs, config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to serve snapshotter")
	}

	if cleanup {
		log.G(ctx).Debug("Closing the snapshotter")
		rs.Close()
	}
	log.G(ctx).Info("Exiting")
}

func serve(ctx context.Context, addr string, rs snapshots.Snapshotter, config snapshotterConfig) (bool, error) {
	// Create a gRPC server
	rpc := grpc.NewServer()

	// Convert the snapshotter to a gRPC service,
	snsvc := snapshotservice.FromSnapshotter(rs)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, snsvc)

	// Prepare the directory for the socket
	if err := os.MkdirAll(filepath.Dir(addr), 0700); err != nil {
		return false, errors.Wrapf(err, "failed to create directory %q", filepath.Dir(addr))
	}

	// Try to remove the socket file to avoid EADDRINUSE
	if err := os.RemoveAll(addr); err != nil {
		return false, errors.Wrapf(err, "failed to remove %q", addr)
	}

	errCh := make(chan error, 1)

	if config.MetricsAddress != "" {
		l, err := net.Listen("tcp", config.MetricsAddress)
		if err != nil {
			return false, errors.Wrapf(err, "failed to get listener for metrics endpoint")
		}
		m := http.NewServeMux()
		m.Handle("/metrics", metrics.Handler())
		go func() {
			if err := http.Serve(l, m); err != nil {
				errCh <- errors.Wrapf(err, "error on serving metrics via socket %q", addr)
			}
		}()
	}

	// Listen and serve
	l, err := net.Listen("unix", addr)
	if err != nil {
		return false, errors.Wrapf(err, "error on listen socket %q", addr)
	}
	go func() {
		if err := rpc.Serve(l); err != nil {
			errCh <- errors.Wrapf(err, "error on serving via socket %q", addr)
		}
	}()

	if os.Getenv("NOTIFY_SOCKET") != "" {
		notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyReady)
		log.G(ctx).Debugf("SdNotifyReady notified=%v, err=%v", notified, notifyErr)
	}
	defer func() {
		if os.Getenv("NOTIFY_SOCKET") != "" {
			notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyStopping)
			log.G(ctx).Debugf("SdNotifyStopping notified=%v, err=%v", notified, notifyErr)
		}
	}()

	var s os.Signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	select {
	case s = <-sigCh:
		log.G(ctx).Infof("Got %v", s)
	case err := <-errCh:
		return false, err
	}
	if s == unix.SIGINT {
		return true, nil // do cleanup on SIGINT
	}
	return false, nil
}
