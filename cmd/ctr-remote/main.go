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

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/cmd/ctr/app"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/commands"
	"github.com/urfave/cli/v3"
)

func main() {
	customCommands := []*cli.Command{
		commands.RpullCommand,
		commands.OptimizeCommand,
		commands.ConvertCommand,
		commands.GetTOCDigestCommand,
		commands.IPFSPushCommand,
	}
	app := app.New()
	for i := range app.Commands {
		if app.Commands[i].Name == "images" {
			sc := map[string]*cli.Command{}
			for _, subcmd := range customCommands {
				sc[subcmd.Name] = subcmd
			}

			// First, replace duplicated subcommands
			for j := range app.Commands[i].Commands {
				for name, subcmd := range sc {
					if name == app.Commands[i].Commands[j].Name {
						app.Commands[i].Commands[j] = subcmd
						delete(sc, name)
					}
				}
			}

			// Next, append all new sub commands
			for _, subcmd := range sc {
				app.Commands[i].Commands = append(app.Commands[i].Commands, subcmd)
			}
			break
		}
	}
	for i := range app.Commands {
		if app.Commands[i].Name == "run" {
			n := 2
			app.Commands[i].StopOnNthArg = &n
			break
		}
	}
	app.Commands = append(app.Commands, commands.FanotifyCommand)
	disableSliceFlagSeparator(app)
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "ctr-remote: %v\n", err)
		os.Exit(1)
	}
}

func disableSliceFlagSeparator(cmd *cli.Command) {
	cmd.DisableSliceFlagSeparator = true
	for _, c := range cmd.Commands {
		disableSliceFlagSeparator(c)
	}
}
