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

package fanotify

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/stargz-snapshotter/analyzer/fanotify/conn"
	"github.com/hashicorp/go-multierror"
)

// Fanotifier monitors "/" mountpoint of a new mount namespace and notifies all
// accessed files.
type Fanotifier struct {
	cmd       *exec.Cmd
	conn      *conn.Client
	closeOnce sync.Once
	closeFunc func() error
}

func SpawnFanotifier(fanotifierBin string) (*Fanotifier, error) {
	// Run fanotifier that monitor "/" in a new mount namespace
	cmd := exec.Command(fanotifierBin, "fanotify", "/")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS,
	}
	notifyR, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	notifyW, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &Fanotifier{
		cmd: cmd,

		// Connect to the spawned fanotifier over stdio
		conn: conn.NewClient(notifyR, notifyW, cmd.Process.Pid, 5*time.Second),
		closeFunc: func() (allErr error) {
			if err := notifyR.Close(); err != nil {
				allErr = multierror.Append(allErr, err)
			}
			if err := notifyW.Close(); err != nil {
				allErr = multierror.Append(allErr, err)
			}
			return
		},
	}, nil
}

// Start starts fanotifier in the new mount namespace
func (n *Fanotifier) Start() error {
	return n.conn.Start()
}

// GetPath gets path notified by the fanotifier.
func (n *Fanotifier) GetPath() (string, error) {
	return n.conn.GetPath()
}

// MountNamespacePath returns the path to the mount namespace where
// the fanotifier is monitoring.
func (n *Fanotifier) MountNamespacePath() string {
	return fmt.Sprintf("/proc/%d/ns/mnt", n.cmd.Process.Pid)
}

// Close kills fanotifier process and closes the connection.
func (n *Fanotifier) Close() (err error) {
	n.closeOnce.Do(func() {
		if err = n.cmd.Process.Kill(); err != nil {
			return
		}
		err = n.closeFunc()
	})
	return
}
