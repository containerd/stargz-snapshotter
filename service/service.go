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
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	stargzfs "github.com/containerd/stargz-snapshotter/fs"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/service/keychain"
	snbase "github.com/containerd/stargz-snapshotter/snapshot"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
)

const (
	fusermountBin            = "fusermount"
	defaultRequestTimeoutSec = 30
)

// NewStargzSnapshotterService returns stargz snapshotter.
func NewStargzSnapshotterService(ctx context.Context, root string, config *Config) (snapshots.Snapshotter, error) {
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

	// Configure filesystem and snapshotter
	fs, err := stargzfs.NewFilesystem(fsRoot(root),
		config.Config,
		stargzfs.WithGetSources(sources(
			sourceFromCRILabels(hosts),      // provides source info based on CRI labels
			source.FromDefaultLabels(hosts), // provides source info based on default labels
		)),
	)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to configure filesystem")
	}

	return snbase.NewSnapshotter(ctx, snapshotterRoot(root), fs, snbase.AsynchronousRemove)
}

func snapshotterRoot(root string) string {
	return filepath.Join(root, "snapshotter")
}

func fsRoot(root string) string {
	return filepath.Join(root, "stargz")
}

func hostsFromConfig(cfg ResolverConfig, credsFuncs ...func(string) (string, string, error)) docker.RegistryHosts {
	return func(host string) (hosts []docker.RegistryHost, _ error) {
		for _, h := range append(cfg.Host[host].Mirrors, MirrorConfig{
			Host: host,
		}) {
			tr := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
			if h.RequestTimeoutSec >= 0 {
				if h.RequestTimeoutSec == 0 {
					tr.Timeout = defaultRequestTimeoutSec * time.Second
				} else {
					tr.Timeout = time.Duration(h.RequestTimeoutSec) * time.Second
				}
			} // h.RequestTimeoutSec < 0 means "no timeout"
			config := docker.RegistryHost{
				Client:       tr,
				Host:         h.Host,
				Scheme:       "https",
				Path:         "/v2",
				Capabilities: docker.HostCapabilityPull,
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

func sources(ps ...source.GetSources) source.GetSources {
	return func(labels map[string]string) (source []source.Source, allErr error) {
		for _, p := range ps {
			src, err := p(labels)
			if err == nil {
				return src, nil
			}
			allErr = multierror.Append(allErr, err)
		}
		return
	}
}

// Supported returns nil when the remote snapshotter is functional on the system with the root directory.
// Supported is not called during plugin initialization, but exposed for downstream projects which uses
// this snapshotter as a library.
func Supported(root string) error {
	// Stargz Snapshotter requires fusermount helper binary.
	if _, err := exec.LookPath(fusermountBin); err != nil {
		return errors.Wrapf(err, "%s not installed", fusermountBin)
	}
	// Remote snapshotter is implemented based on overlayfs snapshotter.
	return overlayutils.Supported(snapshotterRoot(root))
}
