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
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/sys"
	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/fsopts"
	"github.com/containerd/stargz-snapshotter/fusemanager"
	"github.com/containerd/stargz-snapshotter/hardlink"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/keychainconfig"
	snbase "github.com/containerd/stargz-snapshotter/snapshot"
	"github.com/containerd/stargz-snapshotter/version"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	metrics "github.com/docker/go-metrics"
	"github.com/pelletier/go-toml"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
)

const (
	defaultAddress             = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
	defaultConfigPath          = "/etc/containerd-stargz-grpc/config.toml"
	defaultLogLevel            = log.InfoLevel
	defaultRootDir             = "/var/lib/containerd-stargz-grpc"
	defaultImageServiceAddress = "/run/containerd/containerd.sock"
	defaultFuseManagerAddress  = "/run/containerd-stargz-grpc/fuse-manager.sock"
	fuseManagerBin             = "stargz-fuse-manager"
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
	MetricsAddress string `toml:"metrics_address" json:"metrics_address"`

	// NoPrometheus is a flag to disable the emission of the metrics
	NoPrometheus bool `toml:"no_prometheus" json:"no_prometheus"`

	// DebugAddress is a Unix domain socket address where the snapshotter exposes /debug/ endpoints.
	DebugAddress string `toml:"debug_address" json:"debug_address"`

	// IPFS is a flag to enbale lazy pulling from IPFS.
	IPFS bool `toml:"ipfs" json:"ipfs"`

	// MetadataStore is the type of the metadata store to use.
	MetadataStore string `toml:"metadata_store" default:"memory" json:"metadata_store"`

	// FuseManagerConfig is configuration for fusemanager
	FuseManagerConfig `toml:"fuse_manager" json:"fuse_manager"`
}

type FuseManagerConfig struct {
	// Enable is whether detach fusemanager or not
	Enable bool `toml:"enable" default:"false" json:"enable"`

	// Address is address for the fusemanager's GRPC server (default: "/run/containerd-stargz-grpc/fuse-manager.sock")
	Address string `toml:"address" json:"address"`

	// Path is path to the fusemanager's executable (default: looking for a binary "stargz-fuse-manager")
	Path string `toml:"path" json:"path"`
}

func main() {
	rand.Seed(time.Now().UnixNano()) //nolint:staticcheck // Global math/rand seed is deprecated, but still used by external dependencies
	flag.Parse()
	log.SetFormat(log.JSONFormat)
	err := log.SetLevel(*logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	if *printVersion {
		fmt.Println("containerd-stargz-grpc", version.Version, version.Revision)
		return
	}

	var (
		ctx    = log.WithLogger(context.Background(), log.L)
		config snapshotterConfig
	)
	// Streams log of standard lib (go-fuse uses this) into debug log
	// Snapshotter should use "github.com/containerd/log" otherwize
	// logs are always printed as "debug" mode.
	golog.SetOutput(log.G(ctx).WriterLevel(log.DebugLevel))

	// Get configuration from specified file
	tree, err := toml.LoadFile(*configPath)
	if err != nil && (!os.IsNotExist(err) || *configPath != defaultConfigPath) {
		log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
	}
	if err := tree.Unmarshal(&config); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to unmarshal config file %q", *configPath)
	}

	if err := service.Supported(*rootDir); err != nil {
		log.G(ctx).WithError(err).Fatalf("snapshotter is not supported")
	}

	// Initialize global HardlinkManager using snapshotter root
	hlm, err := hardlink.NewHardlinkManager()
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to init hardlink manager")
	}
	hardlink.SetGlobalManager(hlm)

	// Create a gRPC server
	rpc := grpc.NewServer()

	// Configure FUSE passthrough
	// Always set Direct to true to ensure that
	// *directoryCache.Get always return *os.File instead of buffer
	if config.PassThrough {
		config.Direct = true
	}

	// Configure keychain
	keyChainConfig := keychainconfig.Config{
		EnableKubeKeychain:         config.KubeconfigKeychainConfig.EnableKeychain,
		EnableCRIKeychain:          config.CRIKeychainConfig.EnableKeychain,
		KubeconfigPath:             config.KubeconfigPath,
		DefaultImageServiceAddress: defaultImageServiceAddress,
		ImageServicePath:           config.ImageServicePath,
	}

	var rs snapshots.Snapshotter
	fuseManagerConfig := config.FuseManagerConfig
	if fuseManagerConfig.Enable {
		fmPath := fuseManagerConfig.Path
		if fmPath == "" {
			var err error
			fmPath, err = exec.LookPath(fuseManagerBin)
			if err != nil {
				log.G(ctx).WithError(err).Fatalf("failed to find fusemanager bin")
			}
		}
		fmAddr := fuseManagerConfig.Address
		if fmAddr == "" {
			fmAddr = defaultFuseManagerAddress
		}

		if !filepath.IsAbs(fmAddr) {
			log.G(ctx).WithError(err).Fatalf("fuse manager address must be an absolute path: %s", fmAddr)
		}
		managerNewlyStarted, err := fusemanager.StartFuseManager(ctx, fmPath, fmAddr, filepath.Join(*rootDir, "fusestore.db"), *logLevel, filepath.Join(*rootDir, "stargz-fuse-manager.log"))
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to start fusemanager")
		}

		fuseManagerConfig := fusemanager.Config{
			Config:                     config.Config,
			IPFS:                       config.IPFS,
			MetadataStore:              config.MetadataStore,
			DefaultImageServiceAddress: defaultImageServiceAddress,
		}

		fs, err := fusemanager.NewManagerClient(ctx, *rootDir, fmAddr, &fuseManagerConfig)
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure fusemanager")
		}
		flags := []snbase.Opt{snbase.AsynchronousRemove}
		// "managerNewlyStarted" being true indicates that the FUSE manager is newly started. To
		// fully recover the snapshotter and the FUSE manager's state, we need to restore
		// all snapshot mounts. If managerNewlyStarted is false, the existing FUSE manager maintains
		// snapshot mounts so we don't need to restore them.
		if !managerNewlyStarted {
			flags = append(flags, snbase.NoRestore)
		}
		rs, err = snbase.NewSnapshotter(ctx, filepath.Join(*rootDir, "snapshotter"), fs, flags...)
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure snapshotter")
		}
		log.G(ctx).Infof("Start snapshotter with fusemanager mode")
	} else {
		crirpc := rpc
		// For CRI keychain, if listening path is different from stargz-snapshotter's socket, prepare for the dedicated grpc server and the socket.
		serveCRISocket := config.CRIKeychainConfig.EnableKeychain && config.ListenPath != "" && config.ListenPath != *address
		if serveCRISocket {
			crirpc = grpc.NewServer()
		}
		credsFuncs, err := keychainconfig.ConfigKeychain(ctx, crirpc, &keyChainConfig)
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure keychain")
		}
		if serveCRISocket {
			addr := config.ListenPath
			// Prepare the directory for the socket
			if err := os.MkdirAll(filepath.Dir(addr), 0700); err != nil {
				log.G(ctx).WithError(err).Fatalf("failed to create directory %q", filepath.Dir(addr))
			}

			// Try to remove the socket file to avoid EADDRINUSE
			if err := os.RemoveAll(addr); err != nil {
				log.G(ctx).WithError(err).Fatalf("failed to remove %q", addr)
			}

			// Listen and serve
			l, err := net.Listen("unix", addr)
			if err != nil {
				log.G(ctx).WithError(err).Fatalf("error on listen socket %q", addr)
			}
			go func() {
				if err := crirpc.Serve(l); err != nil {
					log.G(ctx).WithError(err).Errorf("error on serving CRI via socket %q", addr)
				}
			}()
		}

		fsConfig := fsopts.Config{
			EnableIpfs:    config.IPFS,
			MetadataStore: config.MetadataStore,
			OpenBoltDB: func(p string) (*bolt.DB, error) {
				return bolt.Open(p, 0600, &bolt.Options{
					NoFreelistSync:  true,
					InitialMmapSize: 64 * 1024 * 1024,
					FreelistType:    bolt.FreelistMapType,
				})
			},
		}
		fsOpts, err := fsopts.ConfigFsOpts(ctx, *rootDir, &fsConfig)
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure fs config")
		}

		rs, err = service.NewStargzSnapshotterService(ctx, *rootDir, &config.Config,
			service.WithCredsFuncs(credsFuncs...), service.WithFilesystemOptions(fsOpts...))
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure snapshotter")
		}
	}

	cleanup, err := serve(ctx, rpc, *address, rs, config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to serve snapshotter")
	}

	// When FUSE manager is disabled, FUSE servers are goroutines in the
	// contaienrd-stargz-grpc process. So killing containerd-stargz-grpc will
	// result in all FUSE mount becoming unavailable with leaving all resources
	// (e.g. temporary cache) on the node. To ensure graceful shutdown, we
	// should always cleanup mounts and associated resources here.
	//
	// When FUSE manager is enabled, those mounts are still under the control by
	// the FUSE manager so we need to avoid cleaning them up unless explicitly
	// commanded via SIGINT. The user can use SIGINT to gracefully killing the FUSE
	// manager before rebooting the node for ensuring that the all snapshots are
	// unmounted with cleaning up associated temporary resources.
	if cleanup || !fuseManagerConfig.Enable {
		log.G(ctx).Debug("Closing the snapshotter")
		rs.Close()
	}
	log.G(ctx).Info("Exiting")
}

func serve(ctx context.Context, rpc *grpc.Server, addr string, rs snapshots.Snapshotter, config snapshotterConfig) (bool, error) {
	// Convert the snapshotter to a gRPC service,
	snsvc := snapshotservice.FromSnapshotter(rs)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, snsvc)

	// Prepare the directory for the socket
	if err := os.MkdirAll(filepath.Dir(addr), 0700); err != nil {
		return false, fmt.Errorf("failed to create directory %q: %w", filepath.Dir(addr), err)
	}

	// Try to remove the socket file to avoid EADDRINUSE
	if err := os.RemoveAll(addr); err != nil {
		return false, fmt.Errorf("failed to remove %q: %w", addr, err)
	}

	errCh := make(chan error, 1)

	// We need to consider both the existence of MetricsAddress as well as NoPrometheus flag not set
	if config.MetricsAddress != "" && !config.NoPrometheus {
		l, err := net.Listen("tcp", config.MetricsAddress)
		if err != nil {
			return false, fmt.Errorf("failed to get listener for metrics endpoint: %w", err)
		}
		m := http.NewServeMux()
		m.Handle("/metrics", metrics.Handler())
		go func() {
			if err := http.Serve(l, m); err != nil {
				errCh <- fmt.Errorf("error on serving metrics via socket %q: %w", addr, err)
			}
		}()
	}

	if config.DebugAddress != "" {
		log.G(ctx).Infof("listen %q for debugging", config.DebugAddress)
		l, err := sys.GetLocalListener(config.DebugAddress, 0, 0)
		if err != nil {
			return false, fmt.Errorf("failed to listen %q: %w", config.DebugAddress, err)
		}
		go func() {
			if err := http.Serve(l, debugServerMux()); err != nil {
				errCh <- fmt.Errorf("error on serving a debug endpoint via socket %q: %w", addr, err)
			}
		}()
	}

	// Listen and serve
	l, err := net.Listen("unix", addr)
	if err != nil {
		return false, fmt.Errorf("error on listen socket %q: %w", addr, err)
	}
	go func() {
		if err := rpc.Serve(l); err != nil {
			errCh <- fmt.Errorf("error on serving via socket %q: %w", addr, err)
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
