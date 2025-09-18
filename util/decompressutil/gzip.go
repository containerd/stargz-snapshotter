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

package decompressutil

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/containerd/stargz-snapshotter/estargz"
)

var (
	findCmdOnce sync.Once
	gzipPath    string
	pigzPath    string
	igzipPath   string
)

func refreshCmdPath() {
	findCmdOnce.Do(func() {
		gzipPath = findCmdPath("gzip")
		pigzPath = findCmdPath("pigz")
		igzipPath = findCmdPath("igzip")
	})
}

func getCmdGzipHelperFunc(cmdPath string) estargz.GzipHelperFunc {
	return func(in io.Reader) (io.ReadCloser, error) {
		cmd := exec.Command(cmdPath, "-d", "-c")

		readCloser, writer := io.Pipe()
		cmd.Stdin = in
		cmd.Stdout = writer

		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf

		if err := cmd.Start(); err != nil {
			writer.Close()
			return nil, err
		}

		go func() {
			if err := cmd.Wait(); err != nil {
				writer.CloseWithError(fmt.Errorf("gzip helper failed, %s: %s", err, errBuf.String()))
			}
			writer.Close()
		}()

		return readCloser, nil
	}
}

func getGoGzipHelperFunc() estargz.GzipHelperFunc {
	return func(in io.Reader) (io.ReadCloser, error) {
		readCloser, err := gzip.NewReader(in)
		if err != nil {
			return nil, err
		}
		return readCloser, nil
	}
}

func GetGzipHelperFunc(gzipHelper string) (estargz.GzipHelperFunc, error) {
	// refresh the path of available decompression commands
	refreshCmdPath()

	var cmdPath string
	switch gzipHelper {
	case "pigz":
		cmdPath = pigzPath
	case "igzip":
		cmdPath = igzipPath
	case "gzip":
		cmdPath = gzipPath
	default:
		return nil, fmt.Errorf("invalid gzip helper: %s", gzipHelper)
	}

	if cmdPath == "" {
		fmt.Printf("Warning: Gzip helper '%s' not found "+
			"Falling back to Go standard library implementation.\n", gzipHelper)
		return getGoGzipHelperFunc(), nil
	}
	return getCmdGzipHelperFunc(cmdPath), nil
}

func findCmdPath(cmd string) string {
	path, err := exec.LookPath(cmd)
	if err != nil {
		return ""
	}
	return path
}
