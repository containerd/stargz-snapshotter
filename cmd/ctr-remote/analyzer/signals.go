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
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/containerd/console"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

type signalKiller interface {
	Kill(context.Context, syscall.Signal, ...containerd.KillOpts) error
}

type consoleResizer interface {
	Resize(context.Context, uint32, uint32) error
}

// forwardAllSignals forwards received signals to the task until stopped
// or the context is canceled.
func forwardAllSignals(ctx context.Context, task signalKiller) func() {
	sigc := make(chan os.Signal, 128)
	signal.Notify(sigc)

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		defer signal.Stop(sigc)

		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case sig := <-sigc:
				// Don't forward signals used by the Go runtime for preemption.
				if sig == syscall.SIGURG {
					continue
				}
				log.G(ctx).Debugf("forwarding signal %s", sig)

				if err := task.Kill(ctx, sig.(syscall.Signal)); err != nil {
					if errdefs.IsNotFound(err) {
						return
					}
					log.G(ctx).WithError(err).Errorf("forward signal %s", sig)
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			signal.Stop(sigc)
			<-stopped
		})
	}
}

// handleConsoleResize updates the task's terminal dimensions initially
// and whenever the local console is resized.
func handleConsoleResize(ctx context.Context, task consoleResizer, con console.Console) (func(), error) {
	resize := func() error {
		size, err := con.Size()
		if err != nil {
			log.G(ctx).WithError(err).Error("get pty size")
			return nil
		}
		return task.Resize(ctx, uint32(size.Width), uint32(size.Height))
	}

	// Preserve ctr's behavior: an initial resize failure is logged,
	// but only failure to read the console size prevents setup.
	size, err := con.Size()
	if err != nil {
		return nil, err
	}
	if err := task.Resize(ctx, uint32(size.Width), uint32(size.Height)); err != nil {
		log.G(ctx).WithError(err).Error("resize pty")
	}

	sigc := make(chan os.Signal, 16)
	signal.Notify(sigc, unix.SIGWINCH)

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)
		defer signal.Stop(sigc)

		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-sigc:
				if err := resize(); err != nil {
					log.G(ctx).WithError(err).Error("resize pty")
				}
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			signal.Stop(sigc)
			<-stopped
		})
	}, nil
}
