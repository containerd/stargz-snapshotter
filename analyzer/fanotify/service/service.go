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
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/stargz-snapshotter/analyzer/fanotify/conn"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// Serve starts fanotify at target directory and notifies all accessed files.
// Passed io.Reader an io.Writer are used for communicating with the client.
func Serve(target string, r io.Reader, w io.Writer) error {
	sConn := conn.NewService(r, w, 5*time.Second)

	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF, unix.O_RDONLY)
	if err != nil {
		return errors.Wrapf(err, "fanotify_init")
	}

	// This blocks until the client tells us to start monitoring the target mountpoint.
	if err := sConn.WaitStart(); err != nil {
		return errors.Wrapf(err, "waiting for start inst")
	}

	// Start monitoring the target mountpoint.
	if err := unix.FanotifyMark(fd,
		unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT,
		unix.FAN_ACCESS|unix.FAN_OPEN,
		unix.AT_FDCWD,
		target,
	); err != nil {
		return errors.Wrapf(err, "fanotify_mark")
	}

	// Notify "started" state to the client.
	if err := sConn.SendStarted(); err != nil {
		return errors.Wrapf(err, "failed to send started message")
	}

	nr := bufio.NewReader(os.NewFile(uintptr(fd), ""))
	for {
		event := &unix.FanotifyEventMetadata{}
		if err := binary.Read(nr, binary.LittleEndian, event); err != nil {
			if err == io.EOF {
				break
			}
			return errors.Wrapf(err, "read fanotify fd")
		}
		if event.Vers != unix.FANOTIFY_METADATA_VERSION {
			return fmt.Errorf("Fanotify version mismatch %d(got) != %d(want)",
				event.Vers, unix.FANOTIFY_METADATA_VERSION)
		}
		if event.Fd < 0 {
			// queue overflow
			// TODO: do we need something special?
			fmt.Fprintf(os.Stderr, "Warn: queue overflow")
			continue
		}

		// Notify file descriptor.
		// NOTE: There is no guarantee that we have /proc in this mount namespace
		// (the target container's rootfs is mounted on "/") so we send file
		// descriptor and let the client resolve the path of this file using /proc of
		// this process.
		if err := sConn.SendFd(int(event.Fd)); err != nil {
			return errors.Wrapf(err, "failed to send fd %d to client", fd)
		}
		if err := unix.Close(int(event.Fd)); err != nil {
			return errors.Wrapf(err, "Close(fd)")
		}

		continue
	}

	return nil
}
