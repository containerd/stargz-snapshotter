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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	golog "log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/log"
	dbmetadata "github.com/containerd/stargz-snapshotter/cmd/containerd-stargz-grpc/db"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/metadata"
	memorymetadata "github.com/containerd/stargz-snapshotter/metadata/memory"
	"github.com/containerd/stargz-snapshotter/service/keychain/kubeconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"github.com/containerd/stargz-snapshotter/store"
	"github.com/containerd/stargz-snapshotter/store/pb"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/pelletier/go-toml"
	bolt "go.etcd.io/bbolt"
	grpc "google.golang.org/grpc"
)

const (
	defaultLogLevel   = log.InfoLevel
	defaultConfigPath = "/etc/stargz-store/config.toml"
	defaultRootDir    = "/var/lib/stargz-store"
)

var (
	configPath = flag.String("config", defaultConfigPath, "path to the configuration file")
	logLevel   = flag.String("log-level", defaultLogLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	rootDir    = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
	listenaddr = flag.String("addr", filepath.Join(defaultRootDir, "store.sock"), "path to the socket listened by this snapshotter")
)

type Config struct {
	config.Config

	// KubeconfigKeychainConfig is config for kubeconfig-based keychain.
	KubeconfigKeychainConfig `toml:"kubeconfig_keychain"`

	// ResolverConfig is config for resolving registries.
	ResolverConfig `toml:"resolver"`

	// MetadataStore is the type of the metadata store to use.
	MetadataStore string `toml:"metadata_store" default:"memory"`
}

type KubeconfigKeychainConfig struct {
	EnableKeychain bool   `toml:"enable_keychain"`
	KubeconfigPath string `toml:"kubeconfig_path"`
}

type ResolverConfig resolver.Config

func main() {
	rand.Seed(time.Now().UnixNano()) //nolint:staticcheck // Global math/rand seed is deprecated, but still used by external dependencies
	flag.Parse()
	mountPoint := flag.Arg(0)
	err := log.SetLevel(*logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	log.SetFormat(log.JSONFormat)
	var (
		ctx    = log.WithLogger(context.Background(), log.L)
		config Config
	)
	// Streams log of standard lib (go-fuse uses this) into debug log
	// Snapshotter should use "github.com/containerd/log" otherwise
	// logs are always printed as "debug" mode.
	golog.SetOutput(log.G(ctx).WriterLevel(log.DebugLevel))

	if mountPoint == "" {
		log.G(ctx).Fatalf("mount point must be specified")
	}

	// Get configuration from specified file
	if *configPath != "" {
		tree, err := toml.LoadFile(*configPath)
		if err != nil && !(os.IsNotExist(err) && *configPath == defaultConfigPath) {
			log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
		}
		if err := tree.Unmarshal(&config); err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to unmarshal config file %q", *configPath)
		}
	}

	sk := new(storeKeychain)

	errCh := serveController(*listenaddr, sk)

	// Prepare kubeconfig-based keychain if required
	credsFuncs := []resolver.Credential{sk.credentials}
	if config.KubeconfigKeychainConfig.EnableKeychain {
		var opts []kubeconfig.Option
		if kcp := config.KubeconfigKeychainConfig.KubeconfigPath; kcp != "" {
			opts = append(opts, kubeconfig.WithKubeconfigPath(kcp))
		}
		credsFuncs = append(credsFuncs, kubeconfig.NewKubeconfigKeychain(ctx, opts...))
	}

	// Use RegistryHosts based on ResolverConfig and keychain
	hosts := resolver.RegistryHostsFromConfig(resolver.Config(config.ResolverConfig), credsFuncs...)

	// Configure and mount filesystem
	if _, err := os.Stat(mountPoint); err != nil {
		if err2 := os.MkdirAll(mountPoint, 0755); err2 != nil && !os.IsExist(err2) {
			log.G(ctx).WithError(err).WithError(err2).
				Fatalf("failed to prepare mountpoint %q", mountPoint)
		}
	}
	if config.Config.DisableVerification {
		log.G(ctx).Fatalf("content verification can't be disabled")
	}
	mt, err := getMetadataStore(*rootDir, config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to configure metadata store")
	}
	layerManager, err := store.NewLayerManager(ctx, *rootDir, hosts, mt, config.Config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to prepare pool")
	}
	if err := store.Mount(ctx, mountPoint, layerManager, config.Config.Debug); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to mount fs at %q", mountPoint)
	}
	defer func() {
		syscall.Unmount(mountPoint, 0)
		log.G(ctx).Info("Exiting")
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

	if err := waitForSignal(ctx, errCh); err != nil {
		log.G(ctx).Errorf("error: %v", err)
		os.Exit(1)
	}
}

func waitForSignal(ctx context.Context, errCh <-chan error) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	select {
	case s := <-c:
		log.G(ctx).Infof("Got %v", s)
	case err := <-errCh:
		return err
	}
	return nil
}

const (
	memoryMetadataType = "memory"
	dbMetadataType     = "db"
)

func getMetadataStore(rootDir string, config Config) (metadata.Store, error) {
	switch config.MetadataStore {
	case "", memoryMetadataType:
		return memorymetadata.NewReader, nil
	case dbMetadataType:
		bOpts := bolt.Options{
			NoFreelistSync:  true,
			InitialMmapSize: 64 * 1024 * 1024,
			FreelistType:    bolt.FreelistMapType,
		}
		db, err := bolt.Open(filepath.Join(rootDir, "metadata.db"), 0600, &bOpts)
		if err != nil {
			return nil, err
		}
		return func(sr *io.SectionReader, opts ...metadata.Option) (metadata.Reader, error) {
			return dbmetadata.NewReader(db, sr, opts...)
		}, nil
	default:
		return nil, fmt.Errorf("unknown metadata store type: %v; must be %v or %v",
			config.MetadataStore, memoryMetadataType, dbMetadataType)
	}
}

func newController(addCredentialFunc func(data []byte) error) *controller {
	return &controller{
		addCredentialFunc: addCredentialFunc,
	}
}

type controller struct {
	addCredentialFunc func(data []byte) error
}

func (c *controller) AddCredential(ctx context.Context, req *pb.AddCredentialRequest) (resp *pb.AddCredentialResponse, _ error) {
	return &pb.AddCredentialResponse{}, c.addCredentialFunc(req.Data)
}

type authConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
}

type storeKeychain struct {
	config   map[string]authConfig
	configMu sync.Mutex
}

func (sk *storeKeychain) add(data []byte) error {
	conf := make(map[string]authConfig)
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&conf); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	sk.configMu.Lock()
	if sk.config == nil {
		sk.config = make(map[string]authConfig)
	}
	for k, c := range conf {
		sk.config[k] = c
	}
	sk.configMu.Unlock()
	return nil
}

func (sk *storeKeychain) credentials(host string, refspec reference.Spec) (string, string, error) {
	if host != refspec.Hostname() {
		return "", "", nil // Do not use creds for mirrors
	}
	sk.configMu.Lock()
	defer sk.configMu.Unlock()
	if acfg, ok := sk.config[refspec.String()]; ok {
		if acfg.IdentityToken != "" {
			return "", acfg.IdentityToken, nil
		} else if !(acfg.Username == "" && acfg.Password == "") {
			return acfg.Username, acfg.Password, nil
		}
	}
	return "", "", nil
}

func serveController(addr string, sk *storeKeychain) <-chan error {
	// Try to remove the socket file to avoid EADDRINUSE
	os.Remove(addr)
	rpc := grpc.NewServer()
	c := newController(sk.add)
	pb.RegisterControllerServer(rpc, c)
	errCh := make(chan error, 1)
	go func() {
		l, err := net.Listen("unix", addr)
		if err != nil {
			errCh <- fmt.Errorf("error on listen socket %q: %w", addr, err)
			return
		}
		if err := rpc.Serve(l); err != nil {
			errCh <- fmt.Errorf("error on serving via socket %q: %w", addr, err)
		}
	}()
	return errCh
}
