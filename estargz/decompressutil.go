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

package estargz

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const disablePrefix = "ESGZ_DISABLE_"

type lookuper func(cmd string) (string, error)

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

func getDecompressCmdPath() string {
	refreshCmdPath()
	// Use decompression commands with priority: pigz > igzip > gzip
	var cmdPath string
	var cmdPaths = []string{
		pigzPath,
		igzipPath,
		gzipPath,
	}
	for _, path := range cmdPaths {
		if path != "" {
			cmdPath = path
			break
		}
	}
	return cmdPath
}

func runCmdDecompress(cmdPath string, in io.Reader, out *os.File) error {
	cmd := exec.Command(cmdPath, "-d", "-c")

	cmd.Stdin = in
	cmd.Stdout = out

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		out.Close()
		os.Remove(out.Name())
		return err
	}

	if err := cmd.Wait(); err != nil {
		out.Close()
		os.Remove(out.Name())
		return fmt.Errorf("%s: %s", err, errBuf.String())
	}

	return nil
}

func findCmdPath(cmd string) string {
	return findCmdPathWithLookuper(cmd, exec.LookPath)
}

func findCmdPathWithLookuper(cmd string, lookuper lookuper) string {
	path, err := lookuper(cmd)
	if err != nil {
		return ""
	}

	// Check if cmd disabled via env variable
	value := os.Getenv(disablePrefix + strings.ToUpper(cmd))
	if value == "" {
		return path
	}

	disable, err := strconv.ParseBool(value)
	if err != nil {
		return path
	}

	if disable {
		return ""
	}

	return path
}
