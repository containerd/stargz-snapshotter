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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/analyzer"
	"github.com/containerd/stargz-snapshotter/converter/optimizer/recorder"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/nativeconverter"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	"github.com/containerd/stargz-snapshotter/nativeconverter/uncompress"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

var ConvertCommand = cli.Command{
	Name:      "convert",
	Usage:     "convert an image",
	ArgsUsage: "[flags] <source_ref> <target_ref>...",
	Description: `Convert an image format.

e.g., 'ctr-remote convert --estargz --oci example.com/foo:orig example.com/foo:esgz'

Use '--platform' to define the output platform.
When '--all-platforms' is given all images in a manifest list must be available.
`,
	Flags: append([]cli.Flag{
		// estargz flags
		cli.BoolFlag{
			Name:  "estargz",
			Usage: "convert legacy tar(.gz) layers to eStargz for lazy pulling. Should be used in conjunction with '--oci'",
		},
		cli.StringFlag{
			Name:  "estargz-record-in",
			Usage: "Read 'ctr-remote optimize --record-out=<FILE>' record file",
		},
		cli.IntFlag{
			Name:  "estargz-compression-level",
			Usage: "eStargz compression level",
			Value: gzip.BestCompression,
		},
		cli.IntFlag{
			Name:  "estargz-chunk-size",
			Usage: "eStargz chunk size",
			Value: 0,
		},
		cli.BoolFlag{
			Name:  "reuse",
			Usage: "reuse eStargz (already optimized) layers without further conversion",
		},
		cli.BoolFlag{
			Name:  "optimize",
			Usage: "optimize image",
		},
		// generic flags
		cli.BoolFlag{
			Name:  "uncompress",
			Usage: "convert tar.gz layers to uncompressed tar layers",
		},
		cli.BoolFlag{
			Name:  "oci",
			Usage: "convert Docker media types to OCI media types",
		},
		// platform flags
		cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform",
			Value: &cli.StringSlice{},
		},
		cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "exports content from all platforms",
		},
	}, samplerFlags...),
	Action: func(clicontext *cli.Context) error {
		var (
			convertOpts = []nativeconverter.ConvertOpt{}
		)
		srcRef := clicontext.Args().Get(0)
		targetRef := clicontext.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
		}

		client, ctx, cancel, err := commands.NewClient(clicontext)
		if err != nil {
			return err
		}
		defer cancel()

		ctx, done, err := client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)

		if !clicontext.Bool("all-platforms") {
			if pss := clicontext.StringSlice("platform"); len(pss) > 0 {
				var all []ocispec.Platform
				for _, ps := range pss {
					p, err := platforms.Parse(ps)
					if err != nil {
						return errors.Wrapf(err, "invalid platform %q", ps)
					}
					all = append(all, p)
				}
				convertOpts = append(convertOpts, nativeconverter.WithPlatform(platforms.Ordered(all...)))
			} else {
				convertOpts = append(convertOpts, nativeconverter.WithPlatform(platforms.Default()))
			}
		}

		if clicontext.Bool("uncompress") {
			convertOpts = append(convertOpts, nativeconverter.WithLayerConvertFunc(uncompress.LayerConvertFunc))
		}

		if clicontext.Bool("oci") {
			convertOpts = append(convertOpts, nativeconverter.WithDockerToOCI(true))
		}

		if clicontext.Bool("estargz") {
			if !clicontext.Bool("oci") {
				logrus.Warn("option --estargz should be used in conjunction with --oci")
			}
			if clicontext.Bool("uncompress") {
				return errors.New("option --estargz conflicts with --uncompress")
			}
			esgzOpts, err := getESGZConvertOpts(clicontext)
			if err != nil {
				return err
			}
			esgzOptsPerLayer, wrapper, err := getESGZConvertOptsPerLayer(ctx, clicontext, client, srcRef)
			if err != nil {
				return err
			}
			f := estargzconvert.LayerConvertWithOptsFunc(esgzOpts, esgzOptsPerLayer)
			if wrapper != nil {
				f = wrapper(f)
			}
			convertOpts = append(convertOpts, nativeconverter.WithLayerConvertFunc(f))
		}

		conv, err := nativeconverter.New(client)
		if err != nil {
			return err
		}
		newImg, err := conv.Convert(ctx, targetRef, srcRef, convertOpts...)
		if err != nil {
			return err
		}
		fmt.Fprintln(clicontext.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}

func getESGZConvertOpts(clicontext *cli.Context) ([]estargz.Option, error) {
	esgzOpts := []estargz.Option{
		estargz.WithCompressionLevel(clicontext.Int("estargz-compression-level")),
		estargz.WithChunkSize(clicontext.Int("estargz-chunk-size")),
	}
	if estargzRecordIn := clicontext.String("estargz-record-in"); estargzRecordIn != "" {
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

func getESGZConvertOptsPerLayer(ctx context.Context, clicontext *cli.Context, client *containerd.Client, srcRef string) (map[digest.Digest][]estargz.Option, func(nativeconverter.ConvertFunc) nativeconverter.ConvertFunc, error) {
	if !clicontext.Bool("optimize") {
		return nil, nil, nil
	}

	// Do analysis only when the target platforms contain the current platform
	if !clicontext.Bool("all-platforms") {
		if pss := clicontext.StringSlice("platform"); len(pss) > 0 {
			containsDefault := false
			for _, ps := range pss {
				p, err := platforms.Parse(ps)
				if err != nil {
					return nil, nil, errors.Wrapf(err, "invalid platform %q", ps)
				}
				if platforms.Default().Match(p) {
					containsDefault = true
				}
			}
			if !containsDefault {
				return nil, nil, nil // do not run analyzer
			}
		}
	}

	// Analyze layers and get prioritized files
	cs := client.ContentStore()
	is := client.ImageService()
	srcImg, err := is.Get(ctx, srcRef)
	if err != nil {
		return nil, nil, err
	}
	descs, decompressedBlobs, config, err := getImage(ctx, cs, srcImg, platforms.Default())
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		for _, ra := range decompressedBlobs {
			ra.Close()
		}
	}()
	samplerOpts, err := getSamplerOpts(clicontext)
	if err != nil {
		return nil, nil, err
	}
	var decompressedRAs []io.ReaderAt
	for _, ra := range decompressedBlobs {
		decompressedRAs = append(decompressedRAs, ra)
	}
	logs, err := analyzer.Analyze(decompressedRAs, config,
		analyzer.WithSamplerOpts(samplerOpts...),
		analyzer.WithPeriod(time.Duration(clicontext.Int("period"))*time.Second),
	)
	if err != nil {
		return nil, nil, err
	}
	var excludes []digest.Digest
	layerOpts := make(map[digest.Digest][]estargz.Option, len(decompressedBlobs))
	for i, desc := range descs {
		if layerLog := logs[i]; layerLog != nil {
			layerOpts[desc.Digest] = []estargz.Option{estargz.WithPrioritizedFiles(layerLog)}
		} else if clicontext.Bool("reuse") && isReusableESGZLayer(ctx, desc, cs) {
			excludes = append(excludes, desc.Digest) // reuse layer without conversion
		}
	}
	return layerOpts, excludeWrapper(excludes), nil
}

func isReusableESGZLayer(ctx context.Context, desc ocispec.Descriptor, cs content.Store) bool {
	dgstStr, ok := desc.Annotations[estargz.TOCJSONDigestAnnotation]
	if !ok {
		return false
	}
	tocdgst, err := digest.Parse(dgstStr)
	if err != nil {
		return false
	}
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return false
	}
	defer ra.Close()
	r, err := estargz.Open(io.NewSectionReader(ra, 0, desc.Size))
	if err != nil {
		return false
	}
	if _, err := r.VerifyTOC(tocdgst); err != nil {
		return false
	}
	return true
}

func excludeWrapper(excludes []digest.Digest) func(nativeconverter.ConvertFunc) nativeconverter.ConvertFunc {
	return func(convertFunc nativeconverter.ConvertFunc) nativeconverter.ConvertFunc {
		return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
			for _, e := range excludes {
				if e == desc.Digest {
					logrus.Warnf("reusing %q without conversion", e)
					return nil, nil
				}
			}
			return convertFunc(ctx, cs, desc)
		}
	}
}

func getImage(ctx context.Context, cs content.Store, srcImg images.Image, platformMC platforms.MatchComparer) ([]ocispec.Descriptor, []content.ReaderAt, ocispec.Image, error) {
	manifest, err := images.Manifest(ctx, cs, srcImg.Target, platformMC)
	if err != nil {
		return nil, nil, ocispec.Image{}, err
	}
	var (
		eg           errgroup.Group
		decompressed = make([]content.ReaderAt, len(manifest.Layers))
		descs        = make([]ocispec.Descriptor, len(manifest.Layers))
	)
	for i, layer := range manifest.Layers {
		i := i
		desc := layer
		descs[i] = layer
		// uncompress layers
		eg.Go(func() error {
			if uDesc, err := uncompress.LayerConvertFunc(ctx, cs, desc); err != nil {
				return err
			} else if uDesc != nil {
				desc = *uDesc
			}
			ra, err := cs.ReaderAt(ctx, desc)
			if err != nil {
				return err
			}
			decompressed[i] = ra
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, nil, ocispec.Image{}, err
	}

	configDesc, err := srcImg.Config(ctx, cs, platformMC)
	if err != nil {
		return nil, nil, ocispec.Image{}, err
	}
	configData, err := content.ReadBlob(ctx, cs, configDesc)
	if err != nil {
		return nil, nil, ocispec.Image{}, err
	}
	var config ocispec.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, nil, ocispec.Image{}, err
	}

	return descs, decompressed, config, nil
}
