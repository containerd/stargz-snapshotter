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

package analyzer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containerd/console"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/cmd/ctr/commands/tasks"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/analyzer/fanotify"
	"github.com/containerd/stargz-snapshotter/analyzer/recorder"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/rs/xid"
)

var defaultPeriod = 10 * time.Second

// Analyze analyzes the passed image then store the record of prioritized files into
// containerd's content store. This function returns the digest of that record file.
// This digest can be used to read record from the content store.
func Analyze(ctx context.Context, client *containerd.Client, ref string, opts ...Option) (digest.Digest, error) {
	var aOpts analyzerOpts
	for _, o := range opts {
		o(&aOpts)
	}
	if aOpts.terminal && aOpts.waitOnSignal {
		return "", fmt.Errorf("wait-on-signal option cannot be used with terminal option")
	}

	target, err := os.MkdirTemp("", "target")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(target)

	cs := client.ContentStore()
	is := client.ImageService()
	ss := client.SnapshotService(aOpts.snapshotter)

	img, err := is.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	platformImg := containerd.NewImageWithPlatform(client, img, platforms.Default())

	// Mount the target image
	// NOTE: We cannot let containerd prepare the rootfs. We create mount namespace *before*
	//       creating the container so containerd's bundle preparation (mounting snapshots to
	//       the bundle directory, etc.) is invisible inside the pre-unshared mount namespace.
	//       This leads to runc using empty directory as the rootfs.
	if unpacked, err := platformImg.IsUnpacked(ctx, aOpts.snapshotter); err != nil {
		return "", err
	} else if !unpacked {
		if err := platformImg.Unpack(ctx, aOpts.snapshotter); err != nil {
			return "", err
		}
	}
	cleanup, err := mountImage(ctx, ss, platformImg, target)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// Spawn a fanotifier process in a new mount namespace and setup recorder.
	fanotifier, err := fanotify.SpawnFanotifier("/proc/self/exe")
	if err != nil {
		return "", fmt.Errorf("failed to spawn fanotifier: %w", err)
	}
	defer func() {
		if err := fanotifier.Close(); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to close fanotifier")
		}
	}()

	// Prepare the spec based on the specified image and runtime options.
	var sOpts []oci.SpecOpts
	if aOpts.specOpts != nil {
		gotOpts, done, err := aOpts.specOpts(platformImg, target)
		if err != nil {
			return "", err
		}
		defer func() {
			if err := done(); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to cleanup container")
				return
			}
		}()
		sOpts = append(sOpts, gotOpts...)
	} else {
		sOpts = append(sOpts,
			oci.WithDefaultSpec(),
			oci.WithDefaultUnixDevices,
			oci.WithRootFSPath(target),
			oci.WithImageConfig(platformImg),
		)
	}
	sOpts = append(sOpts, oci.WithLinuxNamespace(runtimespec.LinuxNamespace{
		Type: runtimespec.MountNamespace,
		Path: fanotifier.MountNamespacePath(), // use mount namespace that the fanotifier created
	}))

	// Create the container and the task
	var container containerd.Container
	for i := 0; i < 3; i++ {
		id := xid.New().String()
		var s runtimespec.Spec
		container, err = client.NewContainer(ctx, id,
			containerd.WithImage(platformImg),
			containerd.WithSnapshotter(aOpts.snapshotter),
			containerd.WithImageStopSignal(platformImg, "SIGKILL"),

			// WithImageConfig depends on WithImage and WithSnapshotter for resolving
			// username (accesses to /etc/{passwd,group} files on the rootfs)
			containerd.WithSpec(&s, sOpts...),
		)
		if err != nil {
			if errdefs.IsAlreadyExists(err) {
				log.G(ctx).WithError(err).Warnf("failed to create container")
				continue
			}
			return "", err
		}
		break
	}
	if container == nil {
		return "", fmt.Errorf("failed to create container")
	}
	defer container.Delete(ctx, containerd.WithSnapshotCleanup)
	var ioCreator cio.Creator
	var con console.Console
	waitLine := newLineWaiter(aOpts.waitLineOut)
	stdinC := newLazyReadCloser(os.Stdin)
	if aOpts.terminal {
		if !aOpts.stdin {
			return "", fmt.Errorf("terminal cannot be used if stdin isn't enabled")
		}
		con = console.Current()
		defer con.Reset()
		if err := con.SetRaw(); err != nil {
			return "", err
		}
		// On terminal mode, the "stderr" field is unused.
		ioCreator = cio.NewCreator(cio.WithStreams(con, waitLine.registerWriter(con), nil), cio.WithTerminal)
	} else if aOpts.stdin {
		ioCreator = cio.NewCreator(cio.WithStreams(stdinC, waitLine.registerWriter(os.Stdout), os.Stderr))
	} else {
		ioCreator = cio.NewCreator(cio.WithStreams(nil, waitLine.registerWriter(os.Stdout), os.Stderr))
	}
	task, err := container.NewTask(ctx, ioCreator)
	if err != nil {
		return "", err
	}
	stdinC.registerCloser(func() { // Ensure to close IO when stdin get EOF
		task.CloseIO(ctx, containerd.WithStdinCloser)
	})

	// Start to monitor "/" and run the task.
	rc, err := recorder.NewImageRecorder(ctx, cs, img, platforms.Default())
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if err := fanotifier.Start(); err != nil {
		return "", fmt.Errorf("failed to start fanotifier: %w", err)
	}
	var fanotifierClosed bool
	var fanotifierClosedMu sync.Mutex
	go func() {
		var successCount int
		defer func() {
			log.G(ctx).Debugf("success record %d path", successCount)
		}()
		for {
			path, err := fanotifier.GetPath()
			if err != nil {
				if err == io.EOF {
					fanotifierClosedMu.Lock()
					isFanotifierClosed := fanotifierClosed
					fanotifierClosedMu.Unlock()
					if isFanotifierClosed {
						break
					}
				}
				log.G(ctx).WithError(err).Error("failed to get notified path")
				break
			}
			if err := rc.Record(path); err != nil {
				log.G(ctx).WithError(err).Debugf("failed to record %q", path)
			}
			successCount++
		}
	}()
	if aOpts.terminal {
		if err := tasks.HandleConsoleResize(ctx, task, con); err != nil {
			log.G(ctx).WithError(err).Error("failed to resize console")
		}
	} else {
		sigc := commands.ForwardAllSignals(ctx, task)
		defer commands.StopCatch(sigc)
	}
	if err := task.Start(ctx); err != nil {
		return "", err
	}

	// Wait until the task exit
	var status containerd.ExitStatus
	var killOk bool
	if aOpts.waitOnSignal { // NOTE: not functional with `terminal` option
		log.G(ctx).Infof("press Ctrl+C to terminate the container")
		status, killOk, err = waitOnSignal(ctx, container, task)
		if err != nil {
			return "", err
		}
	} else {
		if aOpts.period <= 0 {
			aOpts.period = defaultPeriod
		}
		log.G(ctx).Infof("waiting for %v ...", aOpts.period)
		status, killOk, err = waitOnTimeout(ctx, container, task, aOpts.period, waitLine)
		if err != nil {
			return "", err
		}
	}
	if !killOk {
		log.G(ctx).Warnf("failed to exit task %v; manually kill it", task.ID())
	} else {
		code, _, err := status.Result()
		if err != nil {
			return "", err
		}
		log.G(ctx).Infof("container exit with code %v", code)
		if _, err := task.Delete(ctx); err != nil {
			return "", err
		}
	}

	// ensure no record comes in
	fanotifierClosedMu.Lock()
	fanotifierClosed = true
	fanotifierClosedMu.Unlock()
	if err := fanotifier.Close(); err != nil {
		log.G(ctx).WithError(err).Warnf("failed to cleanup fanotifier")
	}

	// Finish recording
	return rc.Commit(ctx)
}

func mountImage(ctx context.Context, ss snapshots.Snapshotter, image containerd.Image, mountpoint string) (func(), error) {
	diffIDs, err := image.RootFS(ctx)
	if err != nil {
		return nil, err
	}
	mounts, err := ss.Prepare(ctx, mountpoint, identity.ChainID(diffIDs).String())
	if err != nil {
		return nil, err
	}
	if err := mount.All(mounts, mountpoint); err != nil {
		if err := ss.Remove(ctx, mountpoint); err != nil && !errdefs.IsNotFound(err) {
			log.G(ctx).WithError(err).Warnf("failed to cleanup snapshot after mount error")
		}
		return nil, fmt.Errorf("failed to mount rootfs at %q: %w", mountpoint, err)
	}
	return func() {
		if err := mount.UnmountAll(mountpoint, 0); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to unmount snapshot")
		}
		if err := ss.Remove(ctx, mountpoint); err != nil && !errdefs.IsNotFound(err) {
			log.G(ctx).WithError(err).Warnf("failed to cleanup snapshot")
		}
	}, nil
}

func waitOnSignal(ctx context.Context, container containerd.Container, task containerd.Task) (containerd.ExitStatus, bool, error) {
	statusC, err := task.Wait(ctx)
	if err != nil {
		return containerd.ExitStatus{}, false, err
	}
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT)
	defer signal.Stop(sc)
	select {
	case status := <-statusC:
		return status, true, nil
	case <-sc:
		log.G(ctx).Info("signal detected")
		status, err := killTask(ctx, container, task, statusC)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("failed to kill container")
			return containerd.ExitStatus{}, false, nil
		}
		return status, true, nil
	}
}

func waitOnTimeout(ctx context.Context, container containerd.Container, task containerd.Task, period time.Duration, line *lineWaiter) (containerd.ExitStatus, bool, error) {
	statusC, err := task.Wait(ctx)
	if err != nil {
		return containerd.ExitStatus{}, false, err
	}
	select {
	case status := <-statusC:
		return status, true, nil
	case l := <-line.waitCh:
		log.G(ctx).Infof("Waiting line detected %q; killing task", l)
	case <-time.After(period):
		log.G(ctx).Warnf("killing task. the time period to monitor access log (%s) has timed out", period.String())
	}
	status, err := killTask(ctx, container, task, statusC)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to kill container")
		return containerd.ExitStatus{}, false, nil
	}
	return status, true, nil
}

func killTask(ctx context.Context, container containerd.Container, task containerd.Task, statusC <-chan containerd.ExitStatus) (containerd.ExitStatus, error) {
	sig, err := containerd.GetStopSignal(ctx, container, syscall.SIGKILL)
	if err != nil {
		return containerd.ExitStatus{}, err
	}
	if err := task.Kill(ctx, sig, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return containerd.ExitStatus{}, fmt.Errorf("forward SIGKILL: %w", err)
	}
	select {
	case status := <-statusC:
		return status, nil
	case <-time.After(5 * time.Second):
		return containerd.ExitStatus{}, fmt.Errorf("timeout")
	}
}

type lazyReadCloser struct {
	reader      io.Reader
	closer      func()
	closerMu    sync.Mutex
	initCond    *sync.Cond
	initialized int64
}

func newLazyReadCloser(r io.Reader) *lazyReadCloser {
	rc := &lazyReadCloser{reader: r, initCond: sync.NewCond(&sync.Mutex{})}
	return rc
}

func (s *lazyReadCloser) registerCloser(closer func()) {
	s.closerMu.Lock()
	s.closer = closer
	s.closerMu.Unlock()
	atomic.AddInt64(&s.initialized, 1)
	s.initCond.Broadcast()
}

func (s *lazyReadCloser) Read(p []byte) (int, error) {
	if atomic.LoadInt64(&s.initialized) <= 0 {
		// wait until initialized
		s.initCond.L.Lock()
		if atomic.LoadInt64(&s.initialized) <= 0 {
			s.initCond.Wait()
		}
		s.initCond.L.Unlock()
	}

	n, err := s.reader.Read(p)
	if err == io.EOF {
		s.closerMu.Lock()
		s.closer()
		s.closerMu.Unlock()
	}
	return n, err
}

func newLineWaiter(s string) *lineWaiter {
	return &lineWaiter{
		waitCh:   make(chan string),
		waitLine: s,
	}
}

type lineWaiter struct {
	waitCh   chan string
	waitLine string
}

func (lw *lineWaiter) registerWriter(w io.Writer) io.Writer {
	if lw.waitLine == "" {
		return w
	}

	pr, pw := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), lw.waitLine) {
				lw.waitCh <- lw.waitLine
			}
		}
		if _, err := io.Copy(io.Discard, pr); err != nil {
			pr.CloseWithError(err)
			return
		}
	}()

	return io.MultiWriter(w, pw)
}
