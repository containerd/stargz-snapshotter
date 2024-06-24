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
	"compress/gzip"
	gocontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	esgzexternaltocconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz/externaltoc"
	zstdchunkedconvert "github.com/containerd/stargz-snapshotter/nativeconverter/zstdchunked"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/klauspost/compress/zstd"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v2"
)

// ConvertCommand converts an image
var ConvertCommand = &cli.Command{
	Name:      "convert",
	Usage:     "convert an image",
	ArgsUsage: "[flags] <source_ref> <target_ref>...",
	Description: `Convert an image format.

e.g., 'ctr-remote convert --estargz --oci example.com/foo:orig example.com/foo:esgz'

Use '--platform' to define the output platform.
When '--all-platforms' is given all images in a manifest list must be available.
`,
	Flags: []cli.Flag{
		// estargz flags
		&cli.BoolFlag{
			Name:  "estargz",
			Usage: "convert legacy tar(.gz) layers to eStargz for lazy pulling. Should be used in conjunction with '--oci'",
		},
		&cli.StringFlag{
			Name:  "estargz-record-in",
			Usage: "Read 'ctr-remote optimize --record-out=<FILE>' record file",
		},
		&cli.IntFlag{
			Name:  "estargz-compression-level",
			Usage: "eStargz compression level",
			Value: gzip.BestCompression,
		},
		&cli.IntFlag{
			Name:  "estargz-chunk-size",
			Usage: "eStargz chunk size",
			Value: 0,
		},
		&cli.IntFlag{
			Name:  "estargz-min-chunk-size",
			Usage: "The minimal number of bytes of data must be written in one gzip stream. Note that this adds a TOC property that old reader doesn't understand.",
			Value: 0,
		},
		&cli.BoolFlag{
			Name:  "estargz-external-toc",
			Usage: "Separate TOC JSON into another image (called \"TOC image\"). The name of TOC image is the original + \"-esgztoc\" suffix. Both eStargz and the TOC image should be pushed to the same registry. stargz-snapshotter refers to the TOC image when it pulls the result eStargz image.",
		},
		&cli.BoolFlag{
			Name:  "estargz-keep-diff-id",
			Usage: "convert to esgz without changing diffID (cannot be used in conjunction with '--estargz-record-in'. must be specified with '--estargz-external-toc')",
		},
		// zstd:chunked flags
		&cli.BoolFlag{
			Name:  "zstdchunked",
			Usage: "use zstd compression instead of gzip (a.k.a zstd:chunked). Must be used in conjunction with '--oci'.",
		},
		&cli.StringFlag{
			Name:  "zstdchunked-record-in",
			Usage: "Read 'ctr-remote optimize --record-out=<FILE>' record file",
		},
		&cli.IntFlag{
			Name:  "zstdchunked-compression-level",
			Usage: "zstd:chunked compression level",
			Value: 3, // SpeedDefault; see also https://pkg.go.dev/github.com/klauspost/compress/zstd#EncoderLevel
		},
		&cli.IntFlag{
			Name:  "zstdchunked-chunk-size",
			Usage: "zstd:chunked chunk size",
			Value: 0,
		},
		// generic flags
		&cli.BoolFlag{
			Name:  "uncompress",
			Usage: "convert tar.gz layers to uncompressed tar layers",
		},
		&cli.BoolFlag{
			Name:  "oci",
			Usage: "convert Docker media types to OCI media types",
		},
		// platform flags
		&cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Convert content for a specific platform",
			Value: &cli.StringSlice{},
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "Convert content for all platforms",
		},
	},
	Action: func(context *cli.Context) error {
		var (
			convertOpts = []converter.Opt{}
		)
		srcRef := context.Args().Get(0)
		targetRef := context.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
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
		convertOpts = append(convertOpts, converter.WithPlatform(platformMC))

		var layerConvertFunc converter.ConvertFunc
		var finalize func(ctx gocontext.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)
		if context.Bool("estargz") {
			esgzOpts, err := getESGZConvertOpts(context)
			if err != nil {
				return err
			}
			if context.Bool("estargz-external-toc") {
				if !context.Bool("estargz-keep-diff-id") {
					layerConvertFunc, finalize = esgzexternaltocconvert.LayerConvertFunc(esgzOpts, context.Int("estargz-compression-level"))
				} else {
					if context.String("estargz-record-in") != "" {
						return fmt.Errorf("option --estargz-keep-diff-id conflicts with --estargz-record-in")
					}
					layerConvertFunc, finalize = esgzexternaltocconvert.LayerConvertLossLessFunc(esgzexternaltocconvert.LayerConvertLossLessConfig{
						CompressionLevel: context.Int("estargz-compression-level"),
						ChunkSize:        context.Int("estargz-chunk-size"),
						MinChunkSize:     context.Int("estargz-min-chunk-size"),
					})
				}
			} else {
				if context.Bool("estargz-keep-diff-id") {
					return fmt.Errorf("option --estargz-keep-diff-id must be used with --estargz-external-toc")
				}
				layerConvertFunc = estargzconvert.LayerConvertFunc(esgzOpts...)
			}
			if !context.Bool("oci") {
				log.L.Warn("option --estargz should be used in conjunction with --oci")
			}
			if context.Bool("uncompress") {
				return errors.New("option --estargz conflicts with --uncompress")
			}
			if context.Bool("zstdchunked") {
				return errors.New("option --estargz conflicts with --zstdchunked")
			}
		}

		if context.Bool("zstdchunked") {
			esgzOpts, err := getZstdchunkedConvertOpts(context)
			if err != nil {
				return err
			}
			layerConvertFunc = zstdchunkedconvert.LayerConvertFuncWithCompressionLevel(
				zstd.EncoderLevelFromZstd(context.Int("zstdchunked-compression-level")), esgzOpts...)
			if !context.Bool("oci") {
				return errors.New("option --zstdchunked must be used in conjunction with --oci")
			}
			if context.Bool("uncompress") {
				return errors.New("option --zstdchunked conflicts with --uncompress")
			}
		}

		if context.Bool("uncompress") {
			layerConvertFunc = uncompress.LayerConvertFunc
		}

		if layerConvertFunc == nil {
			return errors.New("specify layer converter")
		}
		convertOpts = append(convertOpts, converter.WithLayerConvertFunc(layerConvertFunc))

		if context.Bool("oci") {
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
		}

		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		ctx, done, err := client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		go func() {
			// Cleanly cancel conversion
			select {
			case s := <-sigCh:
				log.G(ctx).Infof("Got %v", s)
				cancel()
			case <-ctx.Done():
			}
		}()
		newImg, err := converter.Convert(ctx, client, targetRef, srcRef, convertOpts...)
		if err != nil {
			return err
		}
		if finalize != nil {
			newI, err := finalize(ctx, client.ContentStore(), targetRef, &newImg.Target)
			if err != nil {
				return err
			}
			is := client.ImageService()
			_ = is.Delete(ctx, newI.Name)
			finimg, err := is.Create(ctx, *newI)
			if err != nil {
				return err
			}
			fmt.Fprintln(context.App.Writer, "extra image:", finimg.Name)
		}
		fmt.Fprintln(context.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}

func getESGZConvertOpts(context *cli.Context) ([]estargz.Option, error) {
	esgzOpts := []estargz.Option{
		estargz.WithCompressionLevel(context.Int("estargz-compression-level")),
		estargz.WithChunkSize(context.Int("estargz-chunk-size")),
		estargz.WithMinChunkSize(context.Int("estargz-min-chunk-size")),
	}
	if estargzRecordIn := context.String("estargz-record-in"); estargzRecordIn != "" {
		paths, err := readPathsFromRecordFile(estargzRecordIn)
		if err != nil {
			return nil, err
		}
		esgzOpts = append(esgzOpts, estargz.WithPrioritizedFiles(paths))
		var ignored []string
		esgzOpts = append(esgzOpts, estargz.WithAllowPrioritizeNotFound(&ignored))
	}
	return esgzOpts, nil
}

func getZstdchunkedConvertOpts(context *cli.Context) ([]estargz.Option, error) {
	esgzOpts := []estargz.Option{
		estargz.WithChunkSize(context.Int("zstdchunked-chunk-size")),
	}
	if zstdchunkedRecordIn := context.String("zstdchunked-record-in"); zstdchunkedRecordIn != "" {
		paths, err := readPathsFromRecordFile(zstdchunkedRecordIn)
		if err != nil {
			return nil, err
		}
		esgzOpts = append(esgzOpts, estargz.WithPrioritizedFiles(paths))
		var ignored []string
		esgzOpts = append(esgzOpts, estargz.WithAllowPrioritizeNotFound(&ignored))
	}
	return esgzOpts, nil
}

func readPathsFromRecordFile(filename string) ([]string, error) {
	r, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	dec := json.NewDecoder(r)
	var paths []string
	added := make(map[string]struct{})
	for dec.More() {
		var e recorder.Entry
		if err := dec.Decode(&e); err != nil {
			return nil, err
		}
		if _, ok := added[e.Path]; !ok {
			paths = append(paths, e.Path)
			added[e.Path] = struct{}{}
		}
	}
	return paths, nil
}
