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

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay/overlayutils"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

const (
	targetSnapshotLabel = "containerd.io/snapshot.ref"
	remoteLabel         = "containerd.io/snapshot/remote"
	remoteLabelVal      = "remote snapshot"

	// remoteSnapshotLogKey is a key for log line, which indicates whether
	// `Prepare` method successfully prepared targeting remote snapshot or not, as
	// defined in the following:
	// - "true"  : indicates the snapshot has been successfully prepared as a
	//             remote snapshot
	// - "false" : indicates the snapshot failed to be prepared as a remote
	//             snapshot
	// - null    : undetermined
	remoteSnapshotLogKey = "remote-snapshot-prepared"
	prepareSucceeded     = "true"
	prepareFailed        = "false"
)

// ErrLayerNotRegistered indicates that a remote layer has no live filesystem
// registration. This can happen after snapshotter or node restart when remote
// snapshot restoration was skipped or failed.
var ErrLayerNotRegistered = errors.New("layer not registered")

// FileSystem is a backing filesystem abstraction.
//
// Mount() tries to mount a remote snapshot to the specified mount point
// directory. If succeed, the mountpoint directory will be treated as a layer
// snapshot. If Mount() fails, the mountpoint directory MUST be cleaned up.
// Check() is called to check the connectibity of the existing layer snapshot
// every time the layer is used by containerd.
// Unmount() is called to unmount a remote snapshot from the specified mount point
// directory.
type FileSystem interface {
	Mount(ctx context.Context, mountpoint string, labels map[string]string) error
	Check(ctx context.Context, mountpoint string, labels map[string]string) error
	Unmount(ctx context.Context, mountpoint string) error
}

// SnapshotterConfig is used to configure the remote snapshotter instance
type SnapshotterConfig struct {
	asyncRemove                 bool
	noRestore                   bool
	allowInvalidMountsOnRestart bool
	lazyRestoreOnRestart        bool
}

// Opt is an option to configure the remote snapshotter
type Opt func(config *SnapshotterConfig) error

// AsynchronousRemove defers removal of filesystem content until
// the Cleanup method is called. Removals will make the snapshot
// referred to by the key unavailable and make the key immediately
// available for re-use.
func AsynchronousRemove(config *SnapshotterConfig) error {
	config.asyncRemove = true
	return nil
}

func NoRestore(config *SnapshotterConfig) error {
	config.noRestore = true
	return nil
}

func AllowInvalidMountsOnRestart(config *SnapshotterConfig) error {
	config.allowInvalidMountsOnRestart = true
	return nil
}

// LazyRestoreOnRestart restores remote snapshot directories at startup but
// defers creating their FUSE mounts until the snapshots are used.
func LazyRestoreOnRestart(config *SnapshotterConfig) error {
	config.lazyRestoreOnRestart = true
	return nil
}

type snapshotter struct {
	root        string
	ms          *storage.MetaStore
	asyncRemove bool

	// fs is a filesystem that this snapshotter recognizes.
	fs                          FileSystem
	userxattr                   bool // whether to enable "userxattr" mount option
	noRestore                   bool
	allowInvalidMountsOnRestart bool
	lazyRestoreOnRestart        bool

	remountGroup singleflight.Group
}

// NewSnapshotter returns a Snapshotter which can use unpacked remote layers
// as snapshots. This is implemented based on the overlayfs snapshotter, so
// diffs are stored under the provided root and a metadata file is stored under
// the root as same as overlayfs snapshotter.
func NewSnapshotter(ctx context.Context, root string, targetFs FileSystem, opts ...Opt) (snapshots.Snapshotter, error) {
	if targetFs == nil {
		return nil, fmt.Errorf("specify filesystem to use")
	}

	var config SnapshotterConfig
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	userxattr, err := overlayutils.NeedsUserXAttr(root)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("cannot detect whether \"userxattr\" option needs to be used, assuming to be %v", userxattr)
	}

	o := &snapshotter{
		root:                        root,
		ms:                          ms,
		asyncRemove:                 config.asyncRemove,
		fs:                          targetFs,
		userxattr:                   userxattr,
		noRestore:                   config.noRestore,
		allowInvalidMountsOnRestart: config.allowInvalidMountsOnRestart,
		lazyRestoreOnRestart:        config.lazyRestoreOnRestart,
	}

	if err := o.restoreRemoteSnapshot(ctx); err != nil {
		return nil, fmt.Errorf("failed to restore remote snapshot: %w", err)
	}

	return o, nil
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer t.Rollback()
	_, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		t.Rollback()
		return snapshots.Info{}, err
	}

	if err := t.Commit(); err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

// Usage returns the resources taken by the snapshot identified by key.
//
// For active snapshots, this will scan the usage of the overlay "diff" (aka
// "upper") directory and may take some time.
// for remote snapshots, no scan will be held and recognise the number of inodes
// and these sizes as "zero".
//
// For committed snapshots, the value is returned from the metadata database.
func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Usage{}, err
	}
	id, info, usage, err := storage.GetInfo(ctx, key)
	t.Rollback() // transaction no longer needed at this point.

	if err != nil {
		return snapshots.Usage{}, err
	}

	upperPath := o.upperPath(id)

	if info.Kind == snapshots.KindActive {
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			// TODO(stevvooe): Consider not reporting an error in this case.
			return snapshots.Usage{}, err
		}

		usage = snapshots.Usage(du)
	}

	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	s, err := o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
	if err != nil {
		return nil, err
	}

	// Try to prepare the remote snapshot. If succeeded, we commit the snapshot now
	// and return ErrAlreadyExists.
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, err
		}
	}
	if target, ok := base.Labels[targetSnapshotLabel]; ok {
		// NOTE: If passed labels include a target of the remote snapshot, `Prepare`
		//       must log whether this method succeeded to prepare that remote snapshot
		//       or not, using the key `remoteSnapshotLogKey` defined in the above. This
		//       log is used by tests in this project.
		lCtx := log.WithLogger(ctx, log.G(ctx).WithField("key", key).WithField("parent", parent))
		if err := o.prepareRemoteSnapshot(lCtx, key, base.Labels); err != nil {
			log.G(lCtx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithError(err).Warn("failed to prepare remote snapshot")
		} else {
			base.Labels[remoteLabel] = remoteLabelVal // Mark this snapshot as remote
			err := o.commit(ctx, true, target, key, append(opts, snapshots.WithLabels(base.Labels))...)
			if err == nil || errdefs.IsAlreadyExists(err) {
				// count also AlreadyExists as "success"
				log.G(lCtx).WithField(remoteSnapshotLogKey, prepareSucceeded).Debug("prepared remote snapshot")
				return nil, fmt.Errorf("target snapshot %q: %w", target, errdefs.ErrAlreadyExists)
			}
			log.G(lCtx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithError(err).Warn("failed to internally commit remote snapshot")
			// Don't fallback here (= prohibit to use this key again) because the FileSystem
			// possible has done some work on this "upper" directory.
			return nil, err
		}
	}
	return o.mounts(ctx, s, parent)
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	s, err := o.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
	if err != nil {
		return nil, err
	}
	return o.mounts(ctx, s, parent)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := storage.GetSnapshot(ctx, key)
	t.Rollback()
	if err != nil {
		return nil, fmt.Errorf("failed to get active mount: %w", err)
	}
	return o.mounts(ctx, s, key)
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return o.commit(ctx, false, name, key, opts...)
}

func (o *snapshotter) commit(ctx context.Context, isRemote bool, name, key string, opts ...snapshots.Opt) error {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	rollback := true
	defer func() {
		if rollback {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	// grab the existing id
	id, _, usage, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	if !isRemote { // skip diskusage for remote snapshots for allowing lazy preparation of nodes
		du, err := fs.DiskUsage(ctx, o.upperPath(id))
		if err != nil {
			return err
		}
		usage = snapshots.Usage(du)
	}

	if _, err = storage.CommitActive(ctx, key, name, usage, opts...); err != nil {
		return fmt.Errorf("failed to commit snapshot: %w", err)
	}

	rollback = false
	return t.Commit()
}

// Remove abandons the snapshot identified by key. The snapshot will
// immediately become unavailable and unrecoverable. Disk space will
// be freed up on the next call to `Cleanup`.
func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	_, _, err = storage.Remove(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to remove: %w", err)
	}

	if !o.asyncRemove {
		var removals []string
		const cleanupCommitted = false
		removals, err = o.getCleanupDirectories(ctx, t, cleanupCommitted)
		if err != nil {
			return fmt.Errorf("unable to get directories for removal: %w", err)
		}

		// Remove directories after the transaction is closed, failures must not
		// return error since the transaction is committed with the removal
		// key no longer available.
		defer func() {
			if err == nil {
				for _, dir := range removals {
					if err := o.cleanupSnapshotDirectory(ctx, dir); err != nil {
						log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
					}
				}
			}
		}()

	}

	return t.Commit()
}

// Walk the snapshots.
func (o *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	return storage.WalkInfo(ctx, fn, fs...)
}

// Cleanup cleans up disk resources from removed or abandoned snapshots
func (o *snapshotter) Cleanup(ctx context.Context) error {
	const cleanupCommitted = false
	return o.cleanup(ctx, cleanupCommitted)
}

func (o *snapshotter) cleanup(ctx context.Context, cleanupCommitted bool) error {
	cleanup, err := o.cleanupDirectories(ctx, cleanupCommitted)
	if err != nil {
		return err
	}

	log.G(ctx).Debugf("cleanup: dirs=%v", cleanup)
	for _, dir := range cleanup {
		if err := o.cleanupSnapshotDirectory(ctx, dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}

	return nil
}

func (o *snapshotter) cleanupDirectories(ctx context.Context, cleanupCommitted bool) ([]string, error) {
	// Get a write transaction to ensure no other write transaction can be entered
	// while the cleanup is scanning.
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	defer t.Rollback()
	return o.getCleanupDirectories(ctx, t, cleanupCommitted)
}

func (o *snapshotter) getCleanupDirectories(ctx context.Context, t storage.Transactor, cleanupCommitted bool) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(o.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	remoteSnapshotNames := make(map[string]struct{})
	{
		keyToID := make(map[string]string, len(ids))
		for id, key := range ids {
			keyToID[key] = id
		}
		if err := storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
			if _, ok := info.Labels[remoteLabel]; ok {
				if id, exists := keyToID[info.Name]; exists {
					remoteSnapshotNames[id] = struct{}{}
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	cleanup := []string{}
	for _, d := range dirs {
		if !cleanupCommitted {
			if _, ok := ids[d]; ok {
				continue
			}
		} else {
			if _, ok := remoteSnapshotNames[d]; !ok {
				continue
			}
		}

		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}

	return cleanup, nil
}

func (o *snapshotter) cleanupSnapshotDirectory(ctx context.Context, dir string) error {

	// On a remote snapshot, the layer is mounted on the "fs" directory.
	// We use Filesystem's Unmount API so that it can do necessary finalization
	// before/after the unmount.
	mp := filepath.Join(dir, "fs")
	if err := o.fs.Unmount(ctx, mp); err != nil {
		log.G(ctx).WithError(err).WithField("dir", mp).Debug("failed to unmount")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("failed to remove directory %q: %w", dir, err)
	}
	return nil
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ storage.Snapshot, err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return storage.Snapshot{}, err
	}

	var td, path string
	defer func() {
		if err != nil {
			if td != "" {
				if err1 := o.cleanupSnapshotDirectory(ctx, td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := o.cleanupSnapshotDirectory(ctx, path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	snapshotDir := filepath.Join(o.root, "snapshots")
	td, err = o.prepareDirectory(ctx, snapshotDir, kind)
	if err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
		}
		return storage.Snapshot{}, fmt.Errorf("failed to create prepare snapshot dir: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	s, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return storage.Snapshot{}, fmt.Errorf("failed to create snapshot: %w", err)
	}

	if len(s.ParentIDs) > 0 {
		st, err := os.Stat(o.upperPath(s.ParentIDs[0]))
		if err != nil {
			return storage.Snapshot{}, fmt.Errorf("failed to stat parent: %w", err)
		}

		stat := st.Sys().(*syscall.Stat_t)

		if err := os.Lchown(filepath.Join(td, "fs"), int(stat.Uid), int(stat.Gid)); err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
			return storage.Snapshot{}, fmt.Errorf("failed to chown: %w", err)
		}
	}

	path = filepath.Join(snapshotDir, s.ID)
	if err = os.Rename(td, path); err != nil {
		return storage.Snapshot{}, fmt.Errorf("failed to rename: %w", err)
	}
	td = ""

	rollback = false
	if err = t.Commit(); err != nil {
		return storage.Snapshot{}, fmt.Errorf("commit failed: %w", err)
	}

	return s, nil
}

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}

	return td, nil
}

func (o *snapshotter) mounts(ctx context.Context, s storage.Snapshot, checkKey string) ([]mount.Mount, error) {
	// Make sure that all layers lower than the target layer are available
	if checkKey != "" {
		if err := o.checkAvailability(ctx, checkKey); err != nil {
			return nil, errdefs.ErrUnavailable.WithMessage(fmt.Sprintf("layer %q unavailable: %v", s.ID, err))
		}
	}

	if len(s.ParentIDs) == 0 {
		// if we only have one layer/no parents then just return a bind mount as overlay
		// will not work
		roFlag := "rw"
		if s.Kind == snapshots.KindView {
			roFlag = "ro"
		}

		return []mount.Mount{
			{
				Source: o.upperPath(s.ID),
				Type:   "bind",
				Options: []string{
					roFlag,
					"rbind",
				},
			},
		}, nil
	}
	var options []string

	if s.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", o.workPath(s.ID)),
			fmt.Sprintf("upperdir=%s", o.upperPath(s.ID)),
		)
	} else if len(s.ParentIDs) == 1 {
		return []mount.Mount{
			{
				Source: o.upperPath(s.ParentIDs[0]),
				Type:   "bind",
				Options: []string{
					"ro",
					"rbind",
				},
			},
		}, nil
	}

	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i])
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	if o.userxattr {
		options = append(options, "userxattr")
	}
	return []mount.Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}, nil

}

func (o *snapshotter) upperPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string) string {
	return filepath.Join(o.root, "snapshots", id, "work")
}

// Close closes the snapshotter
func (o *snapshotter) Close() error {
	// unmount all mounts including Committed
	const cleanupCommitted = true
	ctx := context.Background()
	if err := o.cleanup(ctx, cleanupCommitted); err != nil {
		log.G(ctx).WithError(err).Warn("failed to cleanup")
	}

	return o.ms.Close()
}

// prepareRemoteSnapshot tries to prepare the snapshot as a remote snapshot
// using filesystems registered in this snapshotter.
func (o *snapshotter) prepareRemoteSnapshot(ctx context.Context, key string, labels map[string]string) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	id, _, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	mountpoint := o.upperPath(id)
	log.G(ctx).Infof("preparing filesystem mount at mountpoint=%v", mountpoint)

	return o.fs.Mount(ctx, mountpoint, labels)
}

// checkAvailability checks availability of the specified layer and all lower
// layers using filesystem's checking functionality.
func (o *snapshotter) checkAvailability(ctx context.Context, key string) error {
	log.G(ctx).WithField("key", key).Debug("checking layer availability")

	layers, err := o.remoteLayers(ctx, key)
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to collect remote layers")
		return err
	}

	eg, egCtx := errgroup.WithContext(ctx)
	for _, layer := range layers {
		layer := layer
		eg.Go(func() error {
			lCtx := log.WithLogger(egCtx, log.G(egCtx).WithField("mount-point", layer.mountpoint))
			log.G(lCtx).Debug("checking mount point")
			if err := o.ensureLayerAvailable(lCtx, layer.mountpoint, layer.labels); err != nil {
				log.G(lCtx).WithError(err).Warn("layer is unavailable")
				return err
			}
			return nil
		})
	}
	return eg.Wait()
}

type remoteLayer struct {
	mountpoint string
	labels     map[string]string
}

// remoteLayers copies all information needed for availability checks out of
// the metadata transaction so checks and remounts never hold it during network
// operations.
func (o *snapshotter) remoteLayers(ctx context.Context, key string) ([]remoteLayer, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	defer t.Rollback()

	var layers []remoteLayer
	for cKey := key; cKey != ""; {
		id, info, _, err := storage.GetInfo(ctx, cKey)
		if err != nil {
			return nil, fmt.Errorf("failed to get info of %q: %w", cKey, err)
		}
		if _, ok := info.Labels[remoteLabel]; ok {
			layers = append(layers, remoteLayer{
				mountpoint: o.upperPath(id),
				labels:     cloneLabels(info.Labels),
			})
		}
		cKey = info.Parent
	}
	return layers, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func (o *snapshotter) ensureLayerAvailable(ctx context.Context, mountpoint string, labels map[string]string) error {
	err := o.fs.Check(ctx, mountpoint, labels)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrLayerNotRegistered) || !o.lazyRestoreOnRestart {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// A remount is shared by all callers for this mountpoint, so it must not be
	// canceled when one caller goes away. Each caller still observes its own
	// cancellation while waiting for the shared result below.
	remountCtx := context.WithoutCancel(ctx)
	resultCh := o.remountGroup.DoChan(mountpoint, func() (any, error) {
		// Another caller may have completed the mount while this caller waited.
		if err := o.fs.Check(remountCtx, mountpoint, labels); err == nil {
			return nil, nil
		} else if !errors.Is(err, ErrLayerNotRegistered) {
			return nil, err
		}

		log.G(remountCtx).Info("remounting remote layer on demand")
		if err := o.fs.Mount(remountCtx, mountpoint, labels); err != nil {
			return nil, fmt.Errorf("failed to remount remote layer: %w", err)
		}

		return nil, nil
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-resultCh:
		if err := ctx.Err(); err != nil {
			return err
		}
		return result.Err
	}
}

func (o *snapshotter) restoreRemoteSnapshot(ctx context.Context) error {
	if o.noRestore {
		return nil
	}

	mounts, err := mountinfo.GetMounts(nil)
	if err != nil {
		return err
	}
	for _, m := range mounts {
		if strings.HasPrefix(m.Mountpoint, filepath.Join(o.root, "snapshots")) {
			if err := syscall.Unmount(m.Mountpoint, syscall.MNT_FORCE); err != nil {
				return fmt.Errorf("failed to unmount %s: %w", m.Mountpoint, err)
			}
		}
	}

	var task []snapshots.Info
	if err := o.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
		if _, ok := info.Labels[remoteLabel]; ok {
			task = append(task, info)
		}
		return nil
	}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	for _, info := range task {
		// First, prepare the snapshot directory
		if err := func() error {
			ctx, t, err := o.ms.TransactionContext(ctx, false)
			if err != nil {
				return err
			}
			defer t.Rollback()
			id, _, _, err := storage.GetInfo(ctx, info.Name)
			if err != nil {
				return err
			}
			if err := os.Mkdir(filepath.Join(o.root, "snapshots", id), 0700); err != nil && !os.IsExist(err) {
				return err
			}
			if err := os.Mkdir(o.upperPath(id), 0755); err != nil && !os.IsExist(err) {
				return err
			}
			return nil
		}(); err != nil {
			return fmt.Errorf("failed to create remote snapshot directory: %s: %w", info.Name, err)
		}
		if o.lazyRestoreOnRestart {
			log.G(ctx).WithField("snapshot", info.Name).Debug("deferred remote snapshot mount until first use")
			continue
		}
		if err := o.prepareRemoteSnapshot(ctx, info.Name, info.Labels); err != nil {
			if o.allowInvalidMountsOnRestart {
				log.G(ctx).WithError(err).Warnf("failed to restore remote snapshot %s; remove this snapshot manually", info.Name)
				// This snapshot mount is invalid but allow this.
				// NOTE: snapshotter.Mount() will fail to return the mountpoint of these invalid snapshots so
				//       containerd cannot use them anymore. User needs to manually remove the snapshots from
				//       containerd's metadata store using ctr (e.g. `ctr snapshot rm`).
				continue
			}
			return fmt.Errorf("failed to prepare remote snapshot: %s: %w", info.Name, err)
		}
	}

	return nil
}
