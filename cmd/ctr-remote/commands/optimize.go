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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/converter"
	"github.com/containerd/stargz-snapshotter/converter/optimizer"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/imageio"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/recorder"
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
	Flags: append([]cli.Flag{
		cli.BoolFlag{
			Name:  "plain-http",
			Usage: "allow HTTP connections to the registry which has the prefix \"http://\"",
		},
		cli.BoolFlag{
			Name:  "reuse",
			Usage: "reuse eStargz (already optimized) layers without further conversion",
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
		cli.StringFlag{
			Name:  "record-out",
			Usage: "record the monitor log to the specified file",
		},
		// TODO: add "record-in" to use existing record
	}, samplerFlags...),
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
		opts, err := getSamplerOpts(context)
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

		var rec *recorder.Recorder
		if recordOut := context.String("record-out"); recordOut != "" {
			recordWriter, err := os.Create(recordOut)
			if err != nil {
				return err
			}
			defer recordWriter.Close()
			rec = recorder.New(recordWriter)
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
			dstImage, err := converter.ConvertImage(ctx, noOptimize, optimizerOpts, srcImage, &p, tf, rec, opts...)
			if err != nil {
				return err
			}
			return dstIO.WriteImage(dstImage)
		}
		dstIndex, err := converter.ConvertIndex(ctx, noOptimize, optimizerOpts, srcIndex, platform, tf, rec, opts...)
		if err != nil {
			return err
		}
		return dstIO.WriteIndex(dstIndex)
	},
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
