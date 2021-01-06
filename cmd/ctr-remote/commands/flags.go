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
	"encoding/csv"
	"encoding/json"
	"strings"

	"github.com/containerd/stargz-snapshotter/analyzer/sampler"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var samplerFlags = []cli.Flag{
	cli.BoolFlag{
		Name:  "terminal,t",
		Usage: "enable terminal for sample container",
	},
	cli.BoolFlag{
		Name:  "wait-on-signal",
		Usage: "ignore context cancel and keep the container running until it receives signal (Ctrl + C) sent manually",
	},
	cli.IntFlag{
		Name:  "period",
		Usage: "time period to monitor access log",
		Value: defaultPeriod,
	},
	cli.StringFlag{
		Name:  "user",
		Usage: "user name to override image's default config",
	},
	cli.StringFlag{
		Name:  "cwd",
		Usage: "working dir to override image's default config",
	},
	cli.StringFlag{
		Name:  "args",
		Usage: "command arguments to override image's default config(in JSON array)",
	},
	cli.StringFlag{
		Name:  "entrypoint",
		Usage: "entrypoint to override image's default config(in JSON array)",
	},
	cli.StringSliceFlag{
		Name:  "env",
		Usage: "environment valulable to add or override to the image's default config",
	},
	cli.StringSliceFlag{
		Name:  "mount",
		Usage: "additional mounts for the container (e.g. type=foo,source=/path,destination=/target,options=bind)",
	},
	cli.StringFlag{
		Name:  "dns-nameservers",
		Usage: "comma-separated nameservers added to the container's /etc/resolv.conf",
		Value: "8.8.8.8",
	},
	cli.StringFlag{
		Name:  "dns-search-domains",
		Usage: "comma-separated search domains added to the container's /etc/resolv.conf",
	},
	cli.StringFlag{
		Name:  "dns-options",
		Usage: "comma-separated options added to the container's /etc/resolv.conf",
	},
	cli.StringFlag{
		Name:  "add-hosts",
		Usage: "comma-separated hosts configuration (host:IP) added to container's /etc/hosts",
	},
	cli.BoolFlag{
		Name:  "cni",
		Usage: "enable CNI-based networking",
	},
	cli.StringFlag{
		Name:  "cni-plugin-conf-dir",
		Usage: "path to the CNI plugins configuration directory",
	},
	cli.StringFlag{
		Name:  "cni-plugin-dir",
		Usage: "path to the CNI plugins binary directory",
	},
}

func getSamplerOpts(clicontext *cli.Context) (opts []sampler.Option, err error) {
	if env := clicontext.StringSlice("env"); len(env) > 0 {
		opts = append(opts, sampler.WithEnvs(env))
	}
	if mounts := clicontext.StringSlice("mount"); len(mounts) > 0 {
		opts = append(opts, sampler.WithMounts(mounts))
	}
	if args := clicontext.String("args"); args != "" {
		var as []string
		err = json.Unmarshal([]byte(args), &as)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid option \"args\"")
		}
		opts = append(opts, sampler.WithArgs(as))
	}
	if entrypoint := clicontext.String("entrypoint"); entrypoint != "" {
		var es []string
		err = json.Unmarshal([]byte(entrypoint), &es)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid option \"entrypoint\"")
		}
		opts = append(opts, sampler.WithEntrypoint(es))
	}
	if username := clicontext.String("user"); username != "" {
		opts = append(opts, sampler.WithUser(username))
	}
	if cwd := clicontext.String("cwd"); cwd != "" {
		opts = append(opts, sampler.WithWorkingDir(cwd))
	}
	if clicontext.Bool("terminal") {
		opts = append(opts, sampler.WithTerminal())
	}
	if clicontext.Bool("wait-on-signal") {
		opts = append(opts, sampler.WithWaitOnSignal())
	}
	if nameservers := clicontext.String("dns-nameservers"); nameservers != "" {
		fields, err := csv.NewReader(strings.NewReader(nameservers)).Read()
		if err != nil {
			return nil, err
		}
		opts = append(opts, sampler.WithDNSNameservers(fields))
	}
	if search := clicontext.String("dns-search-domains"); search != "" {
		fields, err := csv.NewReader(strings.NewReader(search)).Read()
		if err != nil {
			return nil, err
		}
		opts = append(opts, sampler.WithDNSSearchDomains(fields))
	}
	if dnsopts := clicontext.String("dns-options"); dnsopts != "" {
		fields, err := csv.NewReader(strings.NewReader(dnsopts)).Read()
		if err != nil {
			return nil, err
		}
		opts = append(opts, sampler.WithDNSOptions(fields))
	}
	if hosts := clicontext.String("add-hosts"); hosts != "" {
		fields, err := csv.NewReader(strings.NewReader(hosts)).Read()
		if err != nil {
			return nil, err
		}
		opts = append(opts, sampler.WithExtraHosts(fields))
	}
	if clicontext.Bool("cni") {
		opts = append(opts, sampler.WithCNI())
	}
	if cniPluginConfDir := clicontext.String("cni-plugin-conf-dir"); cniPluginConfDir != "" {
		opts = append(opts, sampler.WithCNIPluginConfDir(cniPluginConfDir))
	}
	if cniPluginDir := clicontext.String("cni-plugin-dir"); cniPluginDir != "" {
		opts = append(opts, sampler.WithCNIPluginDir(cniPluginDir))
	}

	return
}
