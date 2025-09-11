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
	"os"
	"sync"
	"testing"
)

func TestGetGzipHelperFunc(t *testing.T) {
	refreshCmdPath()

	// Detect commands
	allCmds := []string{"gzip", "pigz", "igzip"}
	availableCmds := make(map[string]string)
	unavailableCmds := []string{}

	for _, cmd := range allCmds {
		var path string
		switch cmd {
		case "gzip":
			path = gzipPath
		case "pigz":
			path = pigzPath
		case "igzip":
			path = igzipPath
		}
		if path != "" {
			availableCmds[cmd] = path
		} else {
			unavailableCmds = append(unavailableCmds, cmd)
		}
	}

	if len(availableCmds) == 0 {
		t.Skip("Skipping test: gzip, pigz or igzip commands are not available in environment")
	}

	defer func() {
		// Reset sync.Once to initial state
		findCmdOnce = sync.Once{}
	}()

	testData := []byte("This is test data for verifying GetGzipHelperFunc functionality")

	type testCase struct {
		name         string
		gzipHelper   string
		errorMsg     string
		goGzipReader bool
	}

	tests := []testCase{
		{
			name:         "invalid gzip helper",
			gzipHelper:   "nonexistentcmd",
			errorMsg:     "invalid gzip helper: nonexistentcmd",
			goGzipReader: false,
		},
	}

	// Add test cases according to detected commands
	for cmd := range availableCmds {
		tests = append(tests, testCase{
			name:         fmt.Sprintf("%s gzipHelper", cmd),
			gzipHelper:   cmd,
			errorMsg:     "",
			goGzipReader: false,
		})
	}
	for _, cmd := range unavailableCmds {
		tests = append(tests, testCase{
			name:         fmt.Sprintf("%s gzipHelper but not available", cmd),
			gzipHelper:   cmd,
			errorMsg:     "",
			goGzipReader: true,
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var compressedBuf bytes.Buffer
			gzipWriter := gzip.NewWriter(&compressedBuf)
			_, err := gzipWriter.Write(testData)
			if err != nil {
				t.Fatalf("Failed to compress data: %v", err)
			}
			if err := gzipWriter.Close(); err != nil {
				t.Fatalf("Failed to close gzip writer: %v", err)
			}

			tmpfile, err := os.CreateTemp("", "test-decompress-*")
			if err != nil {
				t.Fatalf("Failed to create temporary file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			gzipHelperFunc, err := GetGzipHelperFunc(test.gzipHelper)
			if err == nil {
				if test.errorMsg != "" {
					t.Fatalf("Expected error: %v", test.errorMsg)
				}
			} else {
				if test.errorMsg != err.Error() {
					t.Fatalf("Error message does not match, expected: %v, actual: %v", test.errorMsg, err.Error())
				} else {
					return
				}
			}

			readCloser, err := gzipHelperFunc(bytes.NewReader(compressedBuf.Bytes()))
			if err != nil {
				t.Fatalf("Failed to run gzip helper function: %v", err)
			}

			if test.goGzipReader {
				if _, ok := readCloser.(*gzip.Reader); !ok {
					t.Errorf("Expected gzipHelperFunc return a *gzip.Reader but got %T", readCloser)
				}
			}
			decompressedData, err := io.ReadAll(readCloser)
			if err != nil {
				t.Fatalf("Failed to read decompressed data: %v", err)
			}

			if !bytes.Equal(decompressedData, testData) {
				t.Errorf("Decompressed data does not match original data")
			}
			err = readCloser.Close()
			if err != nil {
				t.Fatalf("Failed to close gzip read closer: %v", err)
			}
		})
	}
}
