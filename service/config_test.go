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
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestLazyRestoreOnRestartConfig(t *testing.T) {
	data := []byte(`
[snapshotter]
lazy_restore_on_restart = true
allow_invalid_mounts_on_restart = false
`)
	var config Config
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if !config.LazyRestoreOnRestart {
		t.Fatal("lazy_restore_on_restart wasn't decoded")
	}
	if config.AllowInvalidMountsOnRestart {
		t.Fatal("lazy_restore_on_restart unexpectedly enabled allow_invalid_mounts_on_restart")
	}
}
