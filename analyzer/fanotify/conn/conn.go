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

package conn

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	mesStart    = "start"
	mesStarted  = "started"
	mesAck      = "ack"
	mesFdPrefix = "fd:"
)

// Client is the client to talk to the fanotifier.
type Client struct {
	r            io.Reader
	w            io.Writer
	notification *bufio.Scanner
	servicePid   int
	timeout      time.Duration
}

// NewClient returns the client to talk to the fanotifier.
func NewClient(r io.Reader, w io.Writer, servicePid int, timeout time.Duration) *Client {
	return &Client{r, w, bufio.NewScanner(r), servicePid, timeout}
}

// Start lets the notirier start to monitor the filesystem.
func (nc *Client) Start() error {
	if err := writeMessage(nc.w, mesStart); err != nil {
		return err
	}
	if mes, err := scanWithTimeout(nc.notification, nc.timeout); err != nil {
		return err
	} else if mes != mesStarted {
		return fmt.Errorf("non-started message got from fanotifier service: %q", mes)
	}
	return nil
}

// GetPath returns notified path from the fanotifier. This blocks until new path is notified
// from the service
func (nc *Client) GetPath() (string, error) {
	if !nc.notification.Scan() { // NOTE: no timeout
		return "", io.EOF
	}
	mes := nc.notification.Text()
	if !strings.HasPrefix(mes, mesFdPrefix) {
		return "", fmt.Errorf("unexpected prefix for message %q", mes)
	}
	fd, err := strconv.ParseInt(mes[len(mesFdPrefix):], 10, 32)
	if err != nil {
		return "", errors.Wrapf(err, "invalid fd %q", mes)
	}
	path, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", nc.servicePid, fd))
	if err != nil {
		return "", errors.Wrapf(err, "failed to get link from fd %q", mes)
	}
	return path, writeMessage(nc.w, mesAck)
}

// Service is the service end of the fanotifier.
type Service struct {
	r       io.Reader
	w       io.Writer
	client  *bufio.Scanner
	timeout time.Duration
}

// NewService is the service end of the fanotifier.
func NewService(r io.Reader, w io.Writer, timeout time.Duration) *Service {
	return &Service{r, w, bufio.NewScanner(r), timeout}
}

// WaitStart waits "start" message from the client.
func (ns *Service) WaitStart() error {
	if !ns.client.Scan() { // NOTE: no timeout
		return io.EOF
	}
	if mes := ns.client.Text(); mes != mesStart {
		return fmt.Errorf("non-start message got from fanotifier client: %q", mes)
	}
	return nil
}

// SendStarted tells client that the service has been started.
func (ns *Service) SendStarted() error {
	return writeMessage(ns.w, mesStarted)
}

// SendFd send file descriptor to the client.
func (ns *Service) SendFd(fd int) error {
	if err := writeMessage(ns.w, fmt.Sprintf("%s%d", mesFdPrefix, fd)); err != nil {
		return err
	}
	if mes, err := scanWithTimeout(ns.client, ns.timeout); err != nil {
		return err
	} else if mes != mesAck {
		return fmt.Errorf("non-ack message got from fanotifier client: %q", mes)
	}
	return nil
}

func scanWithTimeout(sc *bufio.Scanner, timeout time.Duration) (string, error) {
	notifyCh := make(chan string)
	errCh := make(chan error)
	go func() {
		if !sc.Scan() {
			errCh <- io.EOF
		}
		notifyCh <- sc.Text()
	}()
	select {
	case mes := <-notifyCh:
		return mes, nil
	case err := <-errCh:
		return "", err
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout")
	}
}

func writeMessage(w io.Writer, mes string) error {
	_, err := io.WriteString(w, mes+"\n")
	return err
}
