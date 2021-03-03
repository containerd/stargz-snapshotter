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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/remotes/docker"
	stargzfs "github.com/containerd/stargz-snapshotter/fs"
	"github.com/containerd/stargz-snapshotter/fs/config"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/service/keychain"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/sirupsen/logrus"
)

const (
	defaultLogLevel = logrus.InfoLevel
	defaultRootDir  = "/var/lib/registry-storage"
)

var (
	configPath = flag.String("config", "", "path to the configuration file")
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

type ResolverConfig struct {
	Host map[string]HostConfig `toml:"host"`
}

type HostConfig struct {
	Mirrors []MirrorConfig `toml:"mirrors"`
}

type MirrorConfig struct {
	Host     string `toml:"host"`
	Insecure bool   `toml:"insecure"`
}

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
		if _, err := toml.DecodeFile(*configPath, &config); err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
		}
	}

	// Prepare kubeconfig-based keychain if required
	credsFuncs := []func(string) (string, string, error){keychain.NewDockerconfigKeychain(ctx)}
	if config.KubeconfigKeychainConfig.EnableKeychain {
		var opts []keychain.KubeconfigOption
		if kcp := config.KubeconfigKeychainConfig.KubeconfigPath; kcp != "" {
			opts = append(opts, keychain.WithKubeconfigPath(kcp))
		}
		credsFuncs = append(credsFuncs, keychain.NewKubeconfigKeychain(ctx, opts...))
	}

	// Use RegistryHosts based on ResolverConfig and keychain
	hosts := hostsFromConfig(config.ResolverConfig, credsFuncs...)

	// Configure and mount filesystem
	if err := mountStorage(mountPoint, *rootDir, hosts, config.Config); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to mount fs at %q", mountPoint)
	}
	defer func() {
		syscall.Unmount(mountPoint, 0)
		log.G(ctx).Info("Exiting")
	}()

	waitForSIGINT()
	log.G(ctx).Info("Got SIGINT")
}

func waitForSIGINT() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}

func hostsFromConfig(cfg ResolverConfig, credsFuncs ...func(string) (string, string, error)) docker.RegistryHosts {
	return func(host string) (hosts []docker.RegistryHost, _ error) {
		for _, h := range append(cfg.Host[host].Mirrors, MirrorConfig{
			Host: host,
		}) {
			tr := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
			config := docker.RegistryHost{
				Client:       tr,
				Host:         h.Host,
				Scheme:       "https",
				Path:         "/v2",
				Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve,
				Authorizer: docker.NewDockerAuthorizer(
					docker.WithAuthClient(tr),
					docker.WithAuthCreds(func(host string) (string, string, error) {
						for _, f := range credsFuncs {
							if username, secret, err := f(host); err != nil {
								return "", "", err
							} else if !(username == "" && secret == "") {
								return username, secret, nil
							}
						}
						return "", "", nil
					})),
			}
			if localhost, _ := docker.MatchLocalhost(config.Host); localhost || h.Insecure {
				config.Scheme = "http"
			}
			if config.Host == "docker.io" {
				config.Host = "registry-1.docker.io"
			}
			hosts = append(hosts, config)
		}
		return
	}
}

func mountStorage(mountpoint, root string, hosts docker.RegistryHosts, cfg config.Config) error {
	var (
		fsroot   = filepath.Join(root, "stargz")
		poolroot = filepath.Join(root, "pool")
	)
	if err := os.MkdirAll(fsroot, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(poolroot, 0700); err != nil {
		return err
	}
	sgzfs, err := stargzfs.NewFilesystem(
		fsroot,
		cfg,
		stargzfs.WithGetSources(source.FromDefaultLabels(hosts)),
	)
	if err != nil {
		return err
	}
	timeSec := time.Second
	rawFS := fusefs.NewNodeFS(&rootnode{
		pool: &pool{
			fs:         sgzfs,
			path:       poolroot,
			mounted:    make(map[string]struct{}),
			hosts:      hosts,
			refcounter: make(map[string]map[string]int),
		},
	}, &fusefs.Options{
		AttrTimeout:     &timeSec,
		EntryTimeout:    &timeSec,
		NullPermissions: true,
	})
	server, err := fuse.NewServer(rawFS, mountpoint, &fuse.MountOptions{
		AllowOther: true,             // allow users other than root&mounter to access fs
		Options:    []string{"suid"}, // allow setuid inside container
		Debug:      cfg.Debug,
	})
	if err != nil {
		return err
	}
	go server.Serve()
	return server.WaitMount()
}
