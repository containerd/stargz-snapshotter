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
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/analyzer"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	esgzexternaltocconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz/externaltoc"
	zstdchunkedconvert "github.com/containerd/stargz-snapshotter/nativeconverter/zstdchunked"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v2"
)

const defaultPeriod = 10

// OptimizeCommand converts and optimizes an image
var OptimizeCommand = &cli.Command{
	Name:      "optimize",
	Usage:     "optimize an image with user-specified workload",
	ArgsUsage: "[flags] <source_ref> <target_ref>...",
	Flags: append([]cli.Flag{
		&cli.BoolFlag{
			Name:  "reuse",
			Usage: "reuse eStargz (already optimized) layers without further conversion",
		},
		&cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform",
			Value: &cli.StringSlice{},
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "targeting all platform of the source image",
		},
		&cli.BoolFlag{
			Name:  "wait-on-signal",
			Usage: "ignore context cancel and keep the container running until it receives SIGINT (Ctrl + C) sent manually",
		},
		&cli.StringFlag{
			Name:  "wait-on-line",
			Usage: "Substring of a stdout line to be waited. When this string is detected, the container will be killed.",
		},
		&cli.BoolFlag{
			Name:  "no-optimize",
			Usage: "convert image without optimization",
		},
		&cli.StringFlag{
			Name:  "record-out",
			Usage: "record the monitor log to the specified file",
		},
		&cli.BoolFlag{
			Name:  "oci",
			Usage: "convert Docker media types to OCI media types",
		},
		&cli.IntFlag{
			Name:  "estargz-compression-level",
			Usage: "eStargz compression level",
			Value: gzip.BestCompression,
		},
		&cli.BoolFlag{
			Name:  "estargz-external-toc",
			Usage: "Separate TOC JSON into another image (called \"TOC image\"). The name of TOC image is the original + \"-esgztoc\" suffix. Both eStargz and the TOC image should be pushed to the same registry. stargz-snapshotter refers to the TOC image when it pulls the result eStargz image.",
		},
		&cli.IntFlag{
			Name:  "estargz-chunk-size",
			Usage: "eStargz chunk size (not applied to zstd:chunked)",
			Value: 0,
		},
		&cli.IntFlag{
			Name:  "estargz-min-chunk-size",
			Usage: "The minimal number of bytes of data must be written in one gzip stream. Note that this adds a TOC property that old reader doesn't understand (not applied to zstd:chunked)",
			Value: 0,
		},
		&cli.BoolFlag{
			Name:  "zstdchunked",
			Usage: "use zstd compression instead of gzip (a.k.a zstd:chunked)",
		},
		&cli.IntFlag{
			Name:  "zstdchunked-compression-level",
			Usage: "zstd:chunked compression level",
			Value: 3, // SpeedDefault; see also https://pkg.go.dev/github.com/klauspost/compress/zstd#EncoderLevel
		},
	}, samplerFlags...),
	Action: func(clicontext *cli.Context) error {
		convertOpts := []converter.Opt{}
		srcRef := clicontext.Args().Get(0)
		targetRef := clicontext.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
		}

		var platformMC platforms.MatchComparer
		if clicontext.Bool("all-platforms") {
			platformMC = platforms.All
		} else {
			if pss := clicontext.StringSlice("platform"); len(pss) > 0 {
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

		if clicontext.Bool("oci") {
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
		} else if clicontext.Bool("zstdchunked") {
			return errors.New("option --zstdchunked must be used in conjunction with --oci")
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

		recordOut, esgzOptsPerLayer, wrapper, err := analyze(ctx, clicontext, client, srcRef)
		if err != nil {
			return err
		}
		if recordOutFile := clicontext.String("record-out"); recordOutFile != "" {
			if err := writeContentFile(ctx, client, recordOut, recordOutFile); err != nil {
				return fmt.Errorf("failed output record file: %w", err)
			}
		}
		var f converter.ConvertFunc
		var finalize func(ctx context.Context, cs content.Store, ref string, desc *ocispec.Descriptor) (*images.Image, error)
		if clicontext.Bool("zstdchunked") {
			f = zstdchunkedconvert.LayerConvertWithLayerOptsFuncWithCompressionLevel(
				zstd.EncoderLevelFromZstd(clicontext.Int("zstdchunked-compression-level")), esgzOptsPerLayer)
		} else if !clicontext.Bool("estargz-external-toc") {
			f = estargzconvert.LayerConvertWithLayerAndCommonOptsFunc(esgzOptsPerLayer,
				estargz.WithCompressionLevel(clicontext.Int("estargz-compression-level")),
				estargz.WithChunkSize(clicontext.Int("estargz-chunk-size")),
				estargz.WithMinChunkSize(clicontext.Int("estargz-min-chunk-size")))
		} else {
			if clicontext.Bool("reuse") {
				// We require that the layer conversion is triggerd for each layer
				// to make sure that "finalize" function has the information of all layers.
				return fmt.Errorf("\"estargz-external-toc\" can't be used with \"reuse\" flag")
			}
			f, finalize = esgzexternaltocconvert.LayerConvertWithLayerAndCommonOptsFunc(esgzOptsPerLayer, []estargz.Option{
				estargz.WithChunkSize(clicontext.Int("estargz-chunk-size")),
				estargz.WithMinChunkSize(clicontext.Int("estargz-min-chunk-size")),
			}, clicontext.Int("estargz-compression-level"))
		}
		if wrapper != nil {
			f = wrapper(f)
		}
		layerConvertFunc := logWrapper(f)

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
		convertOpts = append(convertOpts, converter.WithLayerConvertFunc(layerConvertFunc))
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
			fmt.Fprintln(clicontext.App.Writer, "extra image:", finimg.Name)
		}
		fmt.Fprintln(clicontext.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}

func writeContentFile(ctx context.Context, client *containerd.Client, dgst digest.Digest, targetFile string) error {
	fw, err := os.Create(targetFile)
	if err != nil {
		return err
	}
	defer fw.Close()
	ra, err := client.ContentStore().ReaderAt(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		return err
	}
	defer ra.Close()
	_, err = io.Copy(fw, io.NewSectionReader(ra, 0, ra.Size()))
	return err
}

func analyze(ctx context.Context, clicontext *cli.Context, client *containerd.Client, srcRef string) (digest.Digest, map[digest.Digest][]estargz.Option, func(converter.ConvertFunc) converter.ConvertFunc, error) {
	if clicontext.Bool("no-optimize") {
		return "", nil, nil, nil
	}

	// Do analysis only when the target platforms contain the current platform
	if !clicontext.Bool("all-platforms") {
		if pss := clicontext.StringSlice("platform"); len(pss) > 0 {
			containsDefault := false
			for _, ps := range pss {
				p, err := platforms.Parse(ps)
				if err != nil {
					return "", nil, nil, fmt.Errorf("invalid platform %q: %w", ps, err)
				}
				if platforms.DefaultStrict().Match(p) {
					containsDefault = true
				}
			}
			if !containsDefault {
				return "", nil, nil, nil // do not run analyzer
			}
		}
	}

	cs := client.ContentStore()
	is := client.ImageService()

	// Analyze layers and get prioritized files
	aOpts := []analyzer.Option{analyzer.WithSpecOpts(getSpecOpts(clicontext))}
	if clicontext.Bool("wait-on-signal") && clicontext.Bool("terminal") {
		return "", nil, nil, fmt.Errorf("wait-on-signal can't be used with terminal flag")
	}
	if clicontext.Bool("wait-on-signal") {
		aOpts = append(aOpts, analyzer.WithWaitOnSignal())
	} else {
		aOpts = append(aOpts,
			analyzer.WithPeriod(time.Duration(clicontext.Int("period"))*time.Second),
			analyzer.WithWaitLineOut(clicontext.String("wait-on-line")))
	}
	if clicontext.Bool("terminal") {
		if !clicontext.Bool("i") {
			return "", nil, nil, fmt.Errorf("terminal flag must be specified with \"-i\"")
		}
		aOpts = append(aOpts, analyzer.WithTerminal())
	}
	if clicontext.Bool("i") {
		aOpts = append(aOpts, analyzer.WithStdin())
	}
	recordOut, err := analyzer.Analyze(ctx, client, srcRef, aOpts...)
	if err != nil {
		return "", nil, nil, err
	}

	// Parse record file
	srcImg, err := is.Get(ctx, srcRef)
	if err != nil {
		return "", nil, nil, err
	}
	manifestDesc, err := containerdutil.ManifestDesc(ctx, cs, srcImg.Target, platforms.DefaultStrict())
	if err != nil {
		return "", nil, nil, err
	}
	p, err := content.ReadBlob(ctx, cs, manifestDesc)
	if err != nil {
		return "", nil, nil, err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(p, &manifest); err != nil {
		return "", nil, nil, err
	}
	// TODO: this should be indexed by layer "index" (not "digest")
	layerLogs := make(map[digest.Digest][]string, len(manifest.Layers))
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: recordOut})
	if err != nil {
		return "", nil, nil, err
	}
	defer ra.Close()
	dec := json.NewDecoder(io.NewSectionReader(ra, 0, ra.Size()))
	added := make(map[digest.Digest]map[string]struct{}, len(manifest.Layers))
	for dec.More() {
		var e recorder.Entry
		if err := dec.Decode(&e); err != nil {
			return "", nil, nil, err
		}
		if *e.LayerIndex < len(manifest.Layers) &&
			e.ManifestDigest == manifestDesc.Digest.String() {
			dgst := manifest.Layers[*e.LayerIndex].Digest
			if added[dgst] == nil {
				added[dgst] = map[string]struct{}{}
			}
			if _, ok := added[dgst][e.Path]; !ok {
				added[dgst][e.Path] = struct{}{}
				layerLogs[dgst] = append(layerLogs[dgst], e.Path)
			}
		}
	}

	// Create a converter wrapper for skipping layer conversion. This skip occurs
	// if "reuse" option is specified, the source layer is already valid estargz
	// and no access occur to that layer.
	var excludes []digest.Digest
	layerOpts := make(map[digest.Digest][]estargz.Option, len(manifest.Layers))
	for _, desc := range manifest.Layers {
		if layerLog, ok := layerLogs[desc.Digest]; ok && len(layerLog) > 0 {
			layerOpts[desc.Digest] = []estargz.Option{estargz.WithPrioritizedFiles(layerLog)}
		} else if clicontext.Bool("reuse") && isReusableESGZLayer(ctx, desc, cs) {
			excludes = append(excludes, desc.Digest) // reuse layer without conversion
		}
	}
	return recordOut, layerOpts, excludeWrapper(excludes), nil
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
	r, err := estargz.Open(io.NewSectionReader(ra, 0, desc.Size), estargz.WithDecompressors(new(zstdchunked.Decompressor)))
	if err != nil {
		return false
	}
	if _, err := r.VerifyTOC(tocdgst); err != nil {
		return false
	}
	return true
}

func excludeWrapper(excludes []digest.Digest) func(converter.ConvertFunc) converter.ConvertFunc {
	return func(convertFunc converter.ConvertFunc) converter.ConvertFunc {
		return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
			for _, e := range excludes {
				if e == desc.Digest {
					log.G(ctx).Warnf("reusing %q without conversion", e)
					return nil, nil
				}
			}
			return convertFunc(ctx, cs, desc)
		}
	}
}

func logWrapper(convertFunc converter.ConvertFunc) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		log.G(ctx).WithField("digest", desc.Digest).Infof("converting...")
		return convertFunc(ctx, cs, desc)
	}
}
