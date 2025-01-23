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

package cache

import (
	"fmt"
	"os"
	"path/filepath"
)

// BuildCachePath returns the path for a cache entry with the given key
//
//revive:disable:exported
func BuildCachePath(directory string, key string) string {
	return filepath.Join(directory, key[:2], key)
}

//revive:enable:exported

// WipFile creates a temporary file in the given directory with the given key pattern
func WipFile(wipDirectory string, key string) (*os.File, error) {
	if err := os.MkdirAll(wipDirectory, 0700); err != nil {
		return nil, fmt.Errorf("failed to create wip directory: %w", err)
	}
	return os.CreateTemp(wipDirectory, key+"-*")
}
