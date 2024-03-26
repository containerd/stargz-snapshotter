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

package commands

import (
	"fmt"
	"os"

	"github.com/containerd/stargz-snapshotter/analyzer/fanotify/service"
	"github.com/urfave/cli/v2"
)

// FanotifyCommand notifies filesystem event under the specified directory.
var FanotifyCommand = &cli.Command{
	Name:   "fanotify",
	Hidden: true,
	Action: func(context *cli.Context) error {
		target := context.Args().Get(0)
		if target == "" {
			return fmt.Errorf("target must be specified")
		}
		return service.Serve(target, os.Stdin, os.Stdout)
	},
}
