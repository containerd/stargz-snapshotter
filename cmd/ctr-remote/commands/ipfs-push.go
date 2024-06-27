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
	"errors"
	"fmt"

	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/ipfs"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v2"
)

// IPFSPushCommand pushes an image to IPFS
var IPFSPushCommand = &cli.Command{
	Name:      "ipfs-push",
	Usage:     "push an image to IPFS (experimental)",
	ArgsUsage: "[flags] <image_ref>",
	Flags: []cli.Flag{
		// platform flags
		&cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Add content for a specific platform",
			Value: &cli.StringSlice{},
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "Add content for all platforms",
		},
		&cli.BoolFlag{
			Name:  "estargz",
			Value: true,
			Usage: "Convert the image into eStargz",
		},
	},
	Action: func(context *cli.Context) error {
		srcRef := context.Args().Get(0)
		if srcRef == "" {
			return errors.New("image need to be specified")
		}

		var platformMC platforms.MatchComparer
		if context.Bool("all-platforms") {
			platformMC = platforms.All
		} else {
			if pss := context.StringSlice("platform"); len(pss) > 0 {
				var all []ocispec.Platform
				for _, ps := range pss {
					p, err := platforms.Parse(ps)
					if err != nil {
						return fmt.Errorf("invalid platform %q: %w", ps, err)
					}
					all = append(all, p)
				}
				platformMC = platforms.Ordered(all...)
			} else {
				platformMC = platforms.DefaultStrict()
			}
		}

		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		var layerConvert converter.ConvertFunc
		if context.Bool("estargz") {
			layerConvert = estargzconvert.LayerConvertFunc()
		}
		p, err := ipfs.Push(ctx, client, srcRef, layerConvert, platformMC)
		if err != nil {
			return err
		}
		log.L.WithField("CID", p).Infof("Pushed")
		fmt.Println(p)

		return nil
	},
}
