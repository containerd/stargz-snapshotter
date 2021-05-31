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
	golog "log"
	"os"
	"os/signal"
	"syscall"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/service/keychain/dockerconfig"
	"github.com/containerd/stargz-snapshotter/service/keychain/kubeconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"github.com/containerd/stargz-snapshotter/store"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/pelletier/go-toml"
	"github.com/sirupsen/logrus"
)

const (
	defaultLogLevel   = logrus.InfoLevel
	defaultConfigPath = "/etc/stargz-store/config.toml"
	defaultRootDir    = "/var/lib/stargz-store"
)

var (
	configPath = flag.String("config", defaultConfigPath, "path to the configuration file")
	logLevel   = flag.String("log-level", defaultLogLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	rootDir    = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
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

func main() {
	flag.Parse()
	mountPoint := flag.Arg(0)
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	logrus.SetLevel(lvl)
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
	})
	var (
		ctx    = log.WithLogger(context.Background(), log.L)
		config Config
	)
	// Streams log of standard lib (go-fuse uses this) into debug log
	// Snapshotter should use "github.com/containerd/containerd/log" otherwize
	// logs are always printed as "debug" mode.
	golog.SetOutput(log.G(ctx).WriterLevel(logrus.DebugLevel))

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

	// Prepare kubeconfig-based keychain if required
	credsFuncs := []resolver.Credential{dockerconfig.NewDockerconfigKeychain(ctx)}
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
	pool, err := store.NewPool(*rootDir, hosts, config.Config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to prepare pool")
	}
	if err := store.Mount(ctx, mountPoint, pool, config.Config.Debug); err != nil {
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

	waitForSIGINT()
	log.G(ctx).Info("Got SIGINT")
}

func waitForSIGINT() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}
