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
	"strings"
	"sync"
	"testing"
)

func TestFindCmdPath(t *testing.T) {
	mockLookuperFunc := func(mockCmdPath string) lookuper {
		return func(cmd string) (string, error) {
			return mockCmdPath, nil
		}
	}

	tests := []struct {
		name         string
		cmd          string
		envVar       string
		envValue     string
		mockLookuper lookuper
		want         string
	}{
		{
			name:         "command does not exist",
			cmd:          "nonexistentcmd",
			mockLookuper: mockLookuperFunc(""),
			want:         "",
		},
		{
			name:         "command exists and not disabled",
			cmd:          "pigz",
			mockLookuper: mockLookuperFunc("/usr/bin/pigz"),
			envVar:       disablePrefix + "PIGZ",
			envValue:     "",
			want:         "/usr/bin/pigz",
		},
		{
			name:         "command exists but disabled",
			cmd:          "pigz",
			mockLookuper: mockLookuperFunc("/usr/bin/pigz"),
			envVar:       disablePrefix + "PIGZ",
			envValue:     "true",
			want:         "",
		},
		{
			name:         "command exists and environment variable invalid",
			cmd:          "pigz",
			mockLookuper: mockLookuperFunc("/usr/bin/pigz"),
			envVar:       disablePrefix + "PIGZ",
			envValue:     "invalid",
			want:         "/usr/bin/pigz",
		},
		{
			name:         "command exists and explicitly enabled",
			cmd:          "pigz",
			mockLookuper: mockLookuperFunc("/usr/bin/pigz"),
			envVar:       disablePrefix + "PIGZ",
			envValue:     "false",
			want:         "/usr/bin/pigz",
		},
		{
			name:         "igzip command exists and not disabled",
			cmd:          "igzip",
			mockLookuper: mockLookuperFunc("/usr/bin/igzip"),
			want:         "/usr/bin/igzip",
		},
		{
			name:         "igzip command exists but disabled",
			cmd:          "igzip",
			mockLookuper: mockLookuperFunc("/usr/bin/igzip"),
			envVar:       disablePrefix + "IGZIP",
			envValue:     "true",
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVar != "" {
				oldValue := os.Getenv(tt.envVar)
				os.Setenv(tt.envVar, tt.envValue)
				defer os.Setenv(tt.envVar, oldValue)
			}

			if got := findCmdPathWithLookuper(tt.cmd, tt.mockLookuper); got != tt.want {
				t.Errorf("findCmdPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

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

	// Save related environment variables
	origEnv := make(map[string]string)
	for _, cmd := range allCmds {
		envVar := disablePrefix + strings.ToUpper(cmd)
		origEnv[envVar] = os.Getenv(envVar)
	}
	defer func() {
		for envVar, val := range origEnv {
			os.Setenv(envVar, val)
		}
	}()

	defer func() {
		// Reset sync.Once to initial state
		findCmdOnce = sync.Once{}
		printOnce = sync.Once{}
	}()

	testData := []byte("This is test data for verifying GetGzipHelperFunc functionality")

	type testCase struct {
		name         string
		gzipHelper   string
		errorMsg     string
		goGzipReader bool
		envVar       string
		envValue     string
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
		tests = append(tests, testCase{
			name:         fmt.Sprintf("%s gzipHelper but disabled by environment variable", cmd),
			gzipHelper:   cmd,
			errorMsg:     "",
			goGzipReader: true,
			envVar:       disablePrefix + strings.ToUpper(cmd),
			envValue:     "true",
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
			if test.envVar != "" {
				os.Setenv(test.envVar, test.envValue)
			}
			// Set findCmdOnce to new sync.Once to detect commands again
			findCmdOnce = sync.Once{}
			printOnce = sync.Once{}
			refreshCmdPath()

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
