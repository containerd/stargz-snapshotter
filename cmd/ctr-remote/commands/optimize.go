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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package commands

import (
	gocontext "context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/converter"
	"github.com/containerd/stargz-snapshotter/converter/optimizer"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/imageio"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/sampler"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	reglogs "github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	spec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

const defaultPeriod = 10

var OptimizeCommand = cli.Command{
	Name:      "optimize",
	Usage:     "optimize an image with user-specified workload",
	ArgsUsage: "<input-ref> <output-ref>",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "plain-http",
			Usage: "allow HTTP connections to the registry which has the prefix \"http://\"",
		},
		cli.BoolFlag{
			Name:  "reuse",
			Usage: "reuse eStargz (already optimized) layers without further conversion",
		},
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
		cli.StringFlag{
			Name:  "platform",
			Usage: "platform specifier of the source image",
		},
		cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "targeting all platform of the source image",
		},
		cli.BoolFlag{
			Name:  "no-optimize",
			Usage: "convert image without optimization",
		},
	},
	Action: func(context *cli.Context) error {

		ctx := gocontext.Background()

		// Set up logs package of ggcr to get useful messages
		reglogs.Warn.SetOutput(log.G(ctx).WriterLevel(logrus.WarnLevel))
		reglogs.Progress.SetOutput(log.G(ctx).WriterLevel(logrus.InfoLevel))

		// Parse arguments
		var (
			src = context.Args().Get(0)
			dst = context.Args().Get(1)
		)
		if src == "" || dst == "" {
			return fmt.Errorf("source and destination of the target image must be specified")
		}
		opts, err := parseArgs(context)
		if err != nil {
			return errors.Wrap(err, "failed to parse args")
		}

		// Parse references
		srcIO, err := parseReference(src, context)
		if err != nil {
			return errors.Wrapf(err, "failed to parse source ref %q", src)
		}
		dstIO, err := parseReference(dst, context)
		if err != nil {
			return errors.Wrapf(err, "failed to parse destination ref %q", dst)
		}

		// Parse platform information
		var platform *spec.Platform
		if context.Bool("all-platforms") {
			platform = nil
		} else if pStr := context.String("platform"); pStr != "" {
			p, err := platforms.Parse(pStr)
			if err != nil {
				return errors.Wrapf(err, "failed to parse platform %q", pStr)
			}
			platform = &p
		} else {
			p := platforms.DefaultSpec()
			platform = &p
		}

		tf := tempfiles.NewTempFiles()
		defer func() {
			if err := tf.CleanupAll(); err != nil {
				log.G(ctx).WithError(err).Warn("failed to cleanup layer files")
			}
		}()

		noOptimize := context.Bool("no-optimize")
		optimizerOpts := &optimizer.Opts{
			Reuse:  context.Bool("reuse"),
			Period: time.Duration(context.Int("period")) * time.Second,
		}

		// Convert and push the image
		srcIndex, err := srcIO.ReadIndex()
		if err != nil {
			// No index found. Try to deal it as a thin image.
			log.G(ctx).Warn("index not found; treating as a thin image with ignoring the platform option")
			srcImage, err := srcIO.ReadImage()
			if err != nil {
				return err
			}
			p := platforms.DefaultSpec()
			dstImage, err := converter.ConvertImage(ctx, noOptimize, optimizerOpts, srcImage, &p, tf, opts...)
			if err != nil {
				return err
			}
			return dstIO.WriteImage(dstImage)
		}
		dstIndex, err := converter.ConvertIndex(ctx, noOptimize, optimizerOpts, srcIndex, platform, tf, opts...)
		if err != nil {
			return err
		}
		return dstIO.WriteIndex(dstIndex)
	},
}

func parseArgs(clicontext *cli.Context) (opts []sampler.Option, err error) {
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

func parseReference(ref string, clicontext *cli.Context) (imageio.ImageIO, error) {
	if strings.HasPrefix(ref, "local://") {
		abspath, err := filepath.Abs(strings.TrimPrefix(ref, "local://"))
		if err != nil {
			return nil, err
		}
		return &imageio.LocalImage{LocalPath: abspath}, nil
	}
	var opts []name.Option
	if strings.HasPrefix(ref, "http://") {
		ref = strings.TrimPrefix(ref, "http://")
		if clicontext.Bool("plain-http") {
			opts = append(opts, name.Insecure)
		} else {
			return nil, fmt.Errorf("\"--plain-http\" option must be specified to connect to %q using HTTP", ref)
		}
	}
	remoteRef, err := name.ParseReference(ref, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse reference %q", ref)
	}
	return &imageio.RemoteImage{RemoteRef: remoteRef}, nil
}
