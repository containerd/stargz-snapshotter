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
	"os/exec"
	"path/filepath"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	stargzfs "github.com/containerd/stargz-snapshotter/fs"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	snbase "github.com/containerd/stargz-snapshotter/snapshot"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
)

const fusermountBin = "fusermount"

type Option func(*options)

type options struct {
	credsFuncs []resolver.Credential
}

func WithCredsFuncs(creds ...resolver.Credential) Option {
	return func(o *options) {
		o.credsFuncs = append(o.credsFuncs, creds...)
	}
}

// NewStargzSnapshotterService returns stargz snapshotter.
func NewStargzSnapshotterService(ctx context.Context, root string, config *Config, opts ...Option) (snapshots.Snapshotter, error) {
	var sOpts options
	for _, o := range opts {
		o(&sOpts)
	}

	// Use RegistryHosts based on ResolverConfig and keychain
	hosts := resolver.RegistryHostsFromConfig(resolver.Config(config.ResolverConfig), sOpts.credsFuncs...)

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
