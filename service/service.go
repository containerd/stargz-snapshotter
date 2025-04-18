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
	"errors"
	"fmt"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils"
	"github.com/containerd/log"
	stargzfs "github.com/containerd/stargz-snapshotter/fs"
	"github.com/containerd/stargz-snapshotter/fs/layer"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/metadata"
	esgzexternaltoc "github.com/containerd/stargz-snapshotter/nativeconverter/estargz/externaltoc"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"github.com/containerd/stargz-snapshotter/snapshot"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Option func(*options)

type options struct {
	credsFuncs    []resolver.Credential
	registryHosts source.RegistryHosts
	fsOpts        []stargzfs.Option
}

// WithCredsFuncs specifies credsFuncs to be used for connecting to the registries.
func WithCredsFuncs(creds ...resolver.Credential) Option {
	return func(o *options) {
		o.credsFuncs = append(o.credsFuncs, creds...)
	}
}

// WithCustomRegistryHosts is registry hosts to use instead.
func WithCustomRegistryHosts(hosts source.RegistryHosts) Option {
	return func(o *options) {
		o.registryHosts = hosts
	}
}

// WithFilesystemOptions allowes to pass filesystem-related configuration.
func WithFilesystemOptions(opts ...stargzfs.Option) Option {
	return func(o *options) {
		o.fsOpts = opts
	}
}

// NewStargzSnapshotterService returns stargz snapshotter.
func NewStargzSnapshotterService(ctx context.Context, root string, config *Config, opts ...Option) (snapshots.Snapshotter, error) {
	fs, err := NewFileSystem(ctx, root, config, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to configure filesystem: %w", err)
	}

	var snapshotter snapshots.Snapshotter

	snOpts := []snapshot.Opt{snapshot.AsynchronousRemove}
	if config.AllowInvalidMountsOnRestart {
		snOpts = append(snOpts, snapshot.AllowInvalidMountsOnRestart)
	}

	snapshotter, err = snapshot.NewSnapshotter(ctx, snapshotterRoot(root), fs, snOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create new snapshotter: %w", err)
	}

	return snapshotter, nil
}

func NewFileSystem(ctx context.Context, root string, config *Config, opts ...Option) (snapshot.FileSystem, error) {
	var sOpts options
	for _, o := range opts {
		o(&sOpts)
	}

	hosts := sOpts.registryHosts
	if hosts == nil {
		// Use RegistryHosts based on ResolverConfig and keychain
		hosts = resolver.RegistryHostsFromConfig(resolver.Config(config.ResolverConfig), sOpts.credsFuncs...)
	}

	userxattr, err := overlayutils.NeedsUserXAttr(snapshotterRoot(root))
	if err != nil {
		log.G(ctx).WithError(err).Warnf("cannot detect whether \"userxattr\" option needs to be used, assuming to be %v", userxattr)
	}
	opq := layer.OverlayOpaqueTrusted
	if userxattr {
		opq = layer.OverlayOpaqueUser
	}
	// Configure filesystem and snapshotter
	fsOpts := append(sOpts.fsOpts, stargzfs.WithGetSources(sources(
		sourceFromCRILabels(hosts),      // provides source info based on CRI labels
		source.FromDefaultLabels(hosts), // provides source info based on default labels
	)),
		stargzfs.WithOverlayOpaqueType(opq),
		stargzfs.WithAdditionalDecompressors(func(ctx context.Context, hosts source.RegistryHosts, refspec reference.Spec, desc ocispec.Descriptor) []metadata.Decompressor {
			return []metadata.Decompressor{esgzexternaltoc.NewRemoteDecompressor(ctx, hosts, refspec, desc)}
		}),
	)
	fs, err := stargzfs.NewFilesystem(fsRoot(root), config.Config, fsOpts...)
	if err != nil {
		return nil, err
	}

	return fs, nil
}

func snapshotterRoot(root string) string {
	return filepath.Join(root, "snapshotter")
}

func fsRoot(root string) string {
	return filepath.Join(root, "stargz")
}

func sources(ps ...source.GetSources) source.GetSources {
	return func(labels map[string]string) (source []source.Source, allErr error) {
		var errs []error
		for _, p := range ps {
			src, err := p(labels)
			if err == nil {
				return src, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
}

// Supported returns nil when the remote snapshotter is functional on the system with the root directory.
// Supported is not called during plugin initialization, but exposed for downstream projects which uses
// this snapshotter as a library.
func Supported(root string) error {
	// Remote snapshotter is implemented based on overlayfs snapshotter.
	return overlayutils.Supported(snapshotterRoot(root))
}
