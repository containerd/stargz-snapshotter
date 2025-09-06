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
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestRunCmdDecompress(t *testing.T) {
	refreshCmdPath()

	if gzipPath == "" && pigzPath == "" && igzipPath == "" {
		t.Skip("Skipping test: gzip, pigz or igzip commands not available in environment")
	}

	testData := []byte("This is test data for verifying decompression functionality")

	commands := []string{}
	if gzipPath != "" {
		commands = append(commands, gzipPath)
	}
	if pigzPath != "" {
		commands = append(commands, pigzPath)
	}
	if igzipPath != "" {
		commands = append(commands, igzipPath)
	}

	for _, cmd := range commands {
		t.Run("IntegrationTest_"+filepath.Base(cmd), func(t *testing.T) {
			var compressedBuf bytes.Buffer
			compressCmd := exec.Command(cmd, "-c")
			compressCmd.Stdin = bytes.NewReader(testData)
			compressCmd.Stdout = &compressedBuf

			err := compressCmd.Run()
			if err != nil {
				t.Skipf("Cannot compress data using %s: %v", cmd, err)
				return
			}

			tmpfile, err := os.CreateTemp("", "test-decompress-*")
			if err != nil {
				t.Fatalf("Failed to create temporary file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			err = runCmdDecompress(cmd, bytes.NewReader(compressedBuf.Bytes()), tmpfile)
			if err != nil {
				t.Fatalf("Decompression failed: %v", err)
			}

			tmpfile.Seek(0, io.SeekStart)

			decompressedData, err := io.ReadAll(tmpfile)
			if err != nil {
				t.Fatalf("Cannot read decompressed data: %v", err)
			}

			if !bytes.Equal(decompressedData, testData) {
				t.Errorf("Decompressed data does not match original data")
			}
		})
	}
}
