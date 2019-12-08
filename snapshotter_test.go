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
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/handler"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/containerd/snapshots/testsuite"
	fsplugin "github.com/ktock/remote-snapshotter/filesystems"
)

const (
	testTarget               = "testTarget"
	testRef                  = "targetRef"
	testDigest               = "deadbeaf"
	remoteSampleFile         = "foo"
	remoteSampleFileContents = "remote layer"
)

func prepareTarget(t *testing.T, sn snapshots.Snapshotter) string {
	pKey := "/tmp/prepareTarget"
	ctx := context.TODO()
	if _, err := sn.Prepare(ctx, pKey, "", snapshots.WithLabels(map[string]string{
		handler.TargetSnapshotLabel: testTarget,
		handler.TargetRefLabel:      testRef,
		handler.TargetDigestLabel:   testDigest,
	})); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("failed to prepare remote snapshot: %v", err)
	}
	return testTarget
}

func TestRemotePrepare(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sn, err := NewSnapshotter(root, []fsplugin.FileSystem{bindFileSystem(t)})
	if err != nil {
		t.Fatalf("failed to make new remote snapshotter: %q", err)
	}

	// Prepare a remote snapshot.
	target := prepareTarget(t, sn)
	defer sn.Remove(ctx, target)

	// Get internally committed remote snapshot.
	var tinfo *snapshots.Info
	if err := sn.Walk(ctx, func(ctx context.Context, i snapshots.Info) error {
		if tinfo == nil && i.Kind == snapshots.KindCommitted {
			if i.Labels[handler.TargetSnapshotLabel] != target {
				return nil
			}
			if i.Parent != "" {
				return nil
			}
			tinfo = &i
		}
		return nil

	}); err != nil {
		t.Fatalf("failed to get remote snapshot: %v", err)
	}
	if tinfo == nil {
		t.Fatalf("prepared remote snapshot %q not found", target)
	}

	// Stat and validate the remote snapshot.
	info, err := sn.Stat(ctx, tinfo.Name)
	if err != nil {
		t.Fatal("failed to stat remote snapshot")
	}
	if info.Kind != snapshots.KindCommitted {
		t.Errorf("snapshot Kind is %q; want %q", info.Kind, snapshots.KindCommitted)
	}
	if label, ok := info.Labels[handler.TargetSnapshotLabel]; !ok || label != target {
		t.Errorf("remote snapshot hasn't valid remote label: %q", label)
	}
}

func TestRemoteOverlay(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sn, err := NewSnapshotter(root, []fsplugin.FileSystem{bindFileSystem(t)})
	if err != nil {
		t.Fatalf("failed to make new remote snapshotter: %q", err)
	}

	// Prepare a remote snapshot.
	target := prepareTarget(t, sn)
	defer sn.Remove(ctx, target)

	// Prepare a new layer based on the remote snapshot.
	pKey := "/tmp/test"
	mounts, err := sn.Prepare(ctx, pKey, target)
	if err != nil {
		t.Fatalf("faild to prepare using lower remote layer: %v", err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("expected source %q but received %q", "overlay", m.Source)
	}
	var (
		bp    = getBasePath(ctx, sn, root, pKey)
		work  = "workdir=" + filepath.Join(bp, "work")
		upper = "upperdir=" + filepath.Join(bp, "fs")
		lower = "lowerdir=" + getParents(ctx, sn, root, pKey)[0]
	)
	for i, v := range []string{
		work,
		upper,
		lower,
	} {
		if m.Options[i] != v {
			t.Errorf("expected %q but received %q", v, m.Options[i])
		}
	}

	// Validate the contents of the snapshot
	data, err := ioutil.ReadFile(filepath.Join(getParents(ctx, sn, root, pKey)[0], remoteSampleFile))
	if err != nil {
		t.Fatalf("failed to read a file in the remote snapshot: %v", err)
	}
	if e := string(data); e != remoteSampleFileContents {
		t.Fatalf("expected file contents %q but got %q", remoteSampleFileContents, e)
	}
}

func TestFailureDetection(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	fi := bindFileSystem(t)
	sn, err := NewSnapshotter(root, []fsplugin.FileSystem{fi})
	if err != nil {
		t.Fatalf("failed to make new Snapshotter: %q", err)
	}
	fs, ok := fi.(*filesystem)
	if !ok {
		t.Fatalf("Invalid filesystem type(not *filesystem)")
	}

	// Prepare a base remote snapshot.
	target := prepareTarget(t, sn)
	defer sn.Remove(ctx, target)
	pKey := "/tmp/test"

	// Tests if we can detect layer unavailablity on Prepare().
	fs.checkFailure = true
	if _, err := sn.Prepare(ctx, pKey, target); !errdefs.IsUnavailable(err) {
		t.Errorf("got %q; want ErrUnavailable", err)
		return
	}
	sn.Remove(ctx, pKey)

	// Tests if we can detect layer unavailablity on Mounts().
	fs.checkFailure = false
	if _, err := sn.Prepare(ctx, pKey, target); err != nil {
		t.Fatalf("failed to prepare snapshot: %q", err)
	}
	fs.checkFailure = true
	if _, err := sn.Mounts(ctx, pKey); !errdefs.IsUnavailable(err) {
		t.Errorf("got %q; want ErrUnavailable", err)
		return
	}
	sn.Remove(ctx, pKey)
}

func TestRemoteCommit(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	sn, err := NewSnapshotter(root, []fsplugin.FileSystem{bindFileSystem(t)})
	if err != nil {
		t.Fatalf("failed to make new remote snapshotter: %q", err)
	}

	// Prepare a remote snapshot.
	target := prepareTarget(t, sn)
	defer sn.Remove(ctx, target)

	// Prepare a new snapshot based on the remote snapshot
	pKey := "/tmp/test"
	mounts, err := sn.Prepare(ctx, pKey, target)
	if err != nil {
		t.Fatal(err)
	}

	// Make a new active snapshot based on the remote snapshot.
	snapshot, err := ioutil.TempDir("", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(snapshot)
	m := mounts[0]
	if err := m.Mount(snapshot); err != nil {
		t.Fatal(err)
	}
	defer mount.Unmount(snapshot, 0)
	if err := ioutil.WriteFile(filepath.Join(snapshot, "bar"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	mount.Unmount(snapshot, 0)

	// Commit the active snapshot
	cKey := "/tmp/layer"
	if err := sn.Commit(ctx, cKey, pKey); err != nil {
		t.Fatal(err)
	}

	// Validate the committed snapshot
	check, err := ioutil.TempDir("", "check")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(check)
	mounts, err = sn.Prepare(ctx, "/tmp/test2", cKey)
	if err != nil {
		t.Fatal(err)
	}
	m = mounts[0]
	if err := m.Mount(check); err != nil {
		t.Fatal(err)
	}
	defer mount.Unmount(check, 0)
	data, err := ioutil.ReadFile(filepath.Join(check, "bar"))
	if err != nil {
		t.Fatal(err)
	}
	if e := string(data); e != "hi" {
		t.Fatalf("expected file contents %q but got %q", "hi", e)
	}
}

func TestFallback(t *testing.T) {
	tests := []struct {
		name           string
		fs1Failure     bool
		fs2Failure     bool
		fs1mountCalled bool
		fs2mountCalled bool
		notFound       bool
	}{
		{"fs1_serve", false, false, true, false, false}, // fs1 serves
		{"fs2_serve", true, false, true, true, false},   // fs1 fails and fallbacked fs2 serves
		{"fail_all", true, true, true, true, true},      // all filesystems fail
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, err := ioutil.TempDir("", "remote")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			fi1, fi2 := bindFileSystem(t), bindFileSystem(t)
			sn, err := NewSnapshotter(root, []fsplugin.FileSystem{fi1, fi2})
			if err != nil {
				t.Fatalf("failed to make new remote snapshotter: %q", err)
			}

			fs1, ok := fi1.(*filesystem)
			if !ok {
				t.Fatalf("Invalid filesystem type(not *filesystem)")
			}
			fs2, ok := fi2.(*filesystem)
			if !ok {
				t.Fatalf("Invalid filesystem type(not *filesystem)")
			}

			fs1.mountFailure, fs2.mountFailure = tt.fs1Failure, tt.fs2Failure
			fs1.mountCalled, fs2.mountCalled = false, false
			pKey := "/tmp/prepareTarget"
			ctx := context.TODO()
			_, err = sn.Prepare(ctx, pKey, "", snapshots.WithLabels(map[string]string{
				handler.TargetSnapshotLabel: testTarget,
				handler.TargetRefLabel:      testRef,
				handler.TargetDigestLabel:   testDigest,
			}))
			defer sn.Remove(ctx, pKey)
			if !(fs1.mountCalled == tt.fs1mountCalled && fs2.mountCalled == tt.fs2mountCalled) {
				t.Errorf("fs1 called: %t, fs2 called %t; want fs1 called: %t, fs2 called %t",
					fs1.mountCalled, fs2.mountCalled, tt.fs1mountCalled, tt.fs2mountCalled)
				return

			}
			if tt.notFound {
				if errdefs.IsAlreadyExists(err) {
					t.Errorf("succeeded to preprare remote snapshot but want to fail")
					return
				}
			} else if !errdefs.IsAlreadyExists(err) {
				t.Errorf("fail to preprare snapshot but want to success")
				return
			}
		})
	}
}

func bindFileSystem(t *testing.T) fsplugin.FileSystem {
	root, err := ioutil.TempDir("", "remote")
	if err != nil {
		t.Fatalf("failed to prepare working-space for bind filesystem: %q", err)
	}
	if err := ioutil.WriteFile(filepath.Join(root, remoteSampleFile), []byte(remoteSampleFileContents), 0660); err != nil {
		t.Fatalf("failed to write sample file of bind filesystem: %q", err)
	}
	return &filesystem{
		root: root,
		t:    t,
	}
}

type filesystem struct {
	t            *testing.T
	root         string
	mountFailure bool
	checkFailure bool
	mountCalled  bool
}

func (fs *filesystem) Mount(ctx context.Context, ref, digest, mountpoint string) error {
	fs.mountCalled = true
	if fs.mountFailure {
		return fmt.Errorf("failed")
	}
	if ref != testRef || digest != testDigest {
		return fmt.Errorf("layer not found")
	}
	if err := syscall.Mount(fs.root, mountpoint, "none", syscall.MS_BIND, ""); err != nil {
		fs.t.Fatalf("failed to bind mount %q to %q: %v", fs.root, mountpoint, err)
	}
	return nil
}

func (fs *filesystem) Check(ctx context.Context, mountpoint string) error {
	if fs.checkFailure {
		return fmt.Errorf("failed")
	}
	return nil
}

// =============================================================================
// Tests backword-comaptibility of overlayfs snapshotter.

func newSnapshotter(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
	snapshotter, err := NewSnapshotter(root, nil)
	if err != nil {
		return nil, nil, err
	}

	return snapshotter, func() error { return snapshotter.Close() }, nil
}

func TestOverlay(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, "Overlay", newSnapshotter)
}

func TestOverlayMounts(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := o.Prepare(ctx, "/tmp/test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "bind" {
		t.Errorf("mount type should be bind but received %q", m.Type)
	}
	expected := filepath.Join(root, "snapshots", "1", "fs")
	if m.Source != expected {
		t.Errorf("expected source %q but received %q", expected, m.Source)
	}
	if m.Options[0] != "rw" {
		t.Errorf("expected mount option rw but received %q", m.Options[0])
	}
	if m.Options[1] != "rbind" {
		t.Errorf("expected mount option rbind but received %q", m.Options[1])
	}
}

func TestOverlayCommit(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := ioutil.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayOverlayMount(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	if _, err = o.Prepare(ctx, key, ""); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	var mounts []mount.Mount
	if mounts, err = o.Prepare(ctx, "/tmp/layer2", "base"); err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Errorf("should only have 1 mount but received %d", len(mounts))
	}
	m := mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("expected source %q but received %q", "overlay", m.Source)
	}
	var (
		bp    = getBasePath(ctx, o, root, "/tmp/layer2")
		work  = "workdir=" + filepath.Join(bp, "work")
		upper = "upperdir=" + filepath.Join(bp, "fs")
		lower = "lowerdir=" + getParents(ctx, o, root, "/tmp/layer2")[0]
	)
	for i, v := range []string{
		work,
		upper,
		lower,
	} {
		if m.Options[i] != v {
			t.Errorf("expected %q but received %q", v, m.Options[i])
		}
	}
}

func getBasePath(ctx context.Context, sn snapshots.Snapshotter, root, key string) string {
	o := sn.(*snapshotter)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		panic(err)
	}
	defer t.Rollback()

	s, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		panic(err)
	}

	return filepath.Join(root, "snapshots", s.ID)
}

func getParents(ctx context.Context, sn snapshots.Snapshotter, root, key string) []string {
	o := sn.(*snapshotter)
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		panic(err)
	}
	defer t.Rollback()
	s, err := storage.GetSnapshot(ctx, key)
	if err != nil {
		panic(err)
	}
	parents := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parents[i] = filepath.Join(root, "snapshots", s.ParentIDs[i], "fs")
	}
	return parents
}

func TestOverlayOverlayRead(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/test"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := ioutil.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}
	if mounts, err = o.Prepare(ctx, "/tmp/layer2", "base"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "dest")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	if err := mount.All(mounts, dest); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(dest, 0)
	data, err := ioutil.ReadFile(filepath.Join(dest, "foo"))
	if err != nil {
		t.Fatal(err)
	}
	if e := string(data); e != "hi" {
		t.Fatalf("expected file contents hi but got %q", e)
	}
}

func TestOverlayView(t *testing.T) {
	ctx := context.TODO()
	root, err := ioutil.TempDir("", "overlay")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	o, _, err := newSnapshotter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	key := "/tmp/base"
	mounts, err := o.Prepare(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mounts[0]
	if err := ioutil.WriteFile(filepath.Join(m.Source, "foo"), []byte("hi"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "base", key); err != nil {
		t.Fatal(err)
	}

	key = "/tmp/top"
	_, err = o.Prepare(ctx, key, "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(getParents(ctx, o, root, "/tmp/top")[0], "foo"), []byte("hi, again"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := o.Commit(ctx, "top", key); err != nil {
		t.Fatal(err)
	}

	mounts, err = o.View(ctx, "/tmp/view1", "base")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("should only have 1 mount but received %d", len(mounts))
	}
	m = mounts[0]
	if m.Type != "bind" {
		t.Errorf("mount type should be bind but received %q", m.Type)
	}
	expected := getParents(ctx, o, root, "/tmp/view1")[0]
	if m.Source != expected {
		t.Errorf("expected source %q but received %q", expected, m.Source)
	}
	if m.Options[0] != "ro" {
		t.Errorf("expected mount option ro but received %q", m.Options[0])
	}
	if m.Options[1] != "rbind" {
		t.Errorf("expected mount option rbind but received %q", m.Options[1])
	}

	mounts, err = o.View(ctx, "/tmp/view2", "top")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("should only have 1 mount but received %d", len(mounts))
	}
	m = mounts[0]
	if m.Type != "overlay" {
		t.Errorf("mount type should be overlay but received %q", m.Type)
	}
	if m.Source != "overlay" {
		t.Errorf("mount source should be overlay but received %q", m.Source)
	}
	if len(m.Options) != 1 {
		t.Errorf("expected 1 mount option but got %d", len(m.Options))
	}
	lowers := getParents(ctx, o, root, "/tmp/view2")
	expected = fmt.Sprintf("lowerdir=%s:%s", lowers[0], lowers[1])
	if m.Options[0] != expected {
		t.Errorf("expected option %q but received %q", expected, m.Options[0])
	}
}
