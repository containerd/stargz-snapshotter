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
	"io"
	"os"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images/converter"
	"github.com/containerd/containerd/images/converter/uncompress"
	"github.com/containerd/containerd/platforms"
	imgrecorder "github.com/containerd/stargz-snapshotter/analyzer/recorder"
	"github.com/containerd/stargz-snapshotter/estargz"
	estargzconvert "github.com/containerd/stargz-snapshotter/nativeconverter/estargz"
	zstdchunkedconvert "github.com/containerd/stargz-snapshotter/nativeconverter/zstdchunked"
	"github.com/containerd/stargz-snapshotter/recorder"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// ConvertCommand converts an image
var ConvertCommand = cli.Command{
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
		cli.BoolFlag{
			Name:  "estargz",
			Usage: "convert legacy tar(.gz) layers to eStargz for lazy pulling. Should be used in conjunction with '--oci'",
		},
		cli.StringFlag{
			Name:  "estargz-record-in",
			Usage: "Read 'ctr-remote optimize --record-out=<FILE>' record file",
		},
		cli.StringFlag{
			Name:  "estargz-record-in-ref",
			Usage: "Read record file distributed as an image",
		},
		cli.StringFlag{
			Name:  "estargz-record-copy",
			Usage: "Copy record of prioritized files from existing eStargz image",
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
		// zstd:chunked flags
		cli.BoolFlag{
			Name:  "zstdchunked",
			Usage: "use zstd compression instead of gzip (a.k.a zstd:chunked). Must be used in conjunction with '--oci'.",
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
			Usage: "Convert content for a specific platform",
			Value: &cli.StringSlice{},
		},
		cli.BoolFlag{
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
		if context.Bool("estargz") {
			esgzOpts, esgzOptsPerLayer, err := getESGZConvertOpts(ctx, context, client, srcRef, platformMC)
			if err != nil {
				return err
			}
			layerConvertFunc = estargzconvert.LayerConvertWithLayerAndCommonOptsFunc(esgzOptsPerLayer, esgzOpts...)
			if !context.Bool("oci") {
				logrus.Warn("option --estargz should be used in conjunction with --oci")
			}
			if context.Bool("uncompress") {
				return errors.New("option --estargz conflicts with --uncompress")
			}
			if context.Bool("zstdchunked") {
				return errors.New("option --estargz conflicts with --zstdchunked")
			}
		}

		if context.Bool("zstdchunked") {
			esgzOpts, esgzOptsPerLayer, err := getESGZConvertOpts(ctx, context, client, srcRef, platformMC)
			if err != nil {
				return err
			}
			layerConvertFunc = zstdchunkedconvert.LayerConvertWithLayerAndCommonOptsFunc(esgzOptsPerLayer, esgzOpts...)
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
		convertOpts = append(convertOpts, converter.WithLayerConvertFunc(func(ctx gocontext.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
			logrus.Infof("converting blob %q", desc.Digest)
			return layerConvertFunc(ctx, cs, desc)
		}))

		if context.Bool("oci") {
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
		}

		newImg, err := converter.Convert(ctx, client, targetRef, srcRef, convertOpts...)
		if err != nil {
			return err
		}
		fmt.Fprintln(context.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}

func getESGZConvertOpts(ctx gocontext.Context, context *cli.Context, client *containerd.Client, ref string, p platforms.MatchComparer) ([]estargz.Option, map[digest.Digest][]estargz.Option, error) {
	esgzOpts := []estargz.Option{
		estargz.WithCompressionLevel(context.Int("estargz-compression-level")),
		estargz.WithChunkSize(context.Int("estargz-chunk-size")),
	}
	var esgzOptsPerLayer map[digest.Digest][]estargz.Option
	if estargzRecordIn := context.String("estargz-record-in"); estargzRecordIn != "" {
		for _, key := range []string{"estargz-record-in-ref", "estargz-record-copy"} {
			if in := context.String(key); in != "" {
				return nil, nil, fmt.Errorf("\"estargz-record-in\" must not used with %q", key)
			}
		}
		var err error
		esgzOptsPerLayer, err = recordInFromFile(ctx, client, estargzRecordIn, ref, p)
		if err != nil {
			return nil, nil, err
		}
		var ignored []string
		esgzOpts = append(esgzOpts, estargz.WithAllowPrioritizeNotFound(&ignored))
	}
	if estargzRecordInRef := context.String("estargz-record-in-ref"); estargzRecordInRef != "" {
		for _, key := range []string{"estargz-record-in", "estargz-record-copy"} {
			if in := context.String(key); in != "" {
				return nil, nil, fmt.Errorf("\"estargz-record-in-ref\" must not used with %q", key)
			}
		}
		var err error
		esgzOptsPerLayer, err = recordInFromImage(ctx, client, estargzRecordInRef, ref, p)
		if err != nil {
			return nil, nil, err
		}
		var ignored []string
		esgzOpts = append(esgzOpts, estargz.WithAllowPrioritizeNotFound(&ignored))
	}
	if estargzRecordCopyRef := context.String("estargz-record-copy"); estargzRecordCopyRef != "" {
		for _, key := range []string{"estargz-record-in", "estargz-record-in-ref"} {
			if in := context.String(key); in != "" {
				return nil, nil, fmt.Errorf("\"estargz-record-copy\" must not used with %q", key)
			}
		}
		var err error
		esgzOptsPerLayer, err = copyRecordFromImage(ctx, client, estargzRecordCopyRef, ref, p)
		if err != nil {
			return nil, nil, err
		}
		var ignored []string
		esgzOpts = append(esgzOpts, estargz.WithAllowPrioritizeNotFound(&ignored))
	}

	return esgzOpts, esgzOptsPerLayer, nil
}

func recordInFromFile(ctx gocontext.Context, client *containerd.Client, filename, targetImgRef string, platform platforms.MatchComparer) (map[digest.Digest][]estargz.Option, error) {
	r, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return recordInFromReader(ctx, client, r, targetImgRef, platform)
}

func recordInFromImage(ctx gocontext.Context, client *containerd.Client, recordInImgRef, targetImgRef string, platform platforms.MatchComparer) (map[digest.Digest][]estargz.Option, error) {
	recordInDgst, err := imgrecorder.RecordInFromImage(ctx, client, recordInImgRef, platform)
	if err != nil {
		return nil, err
	}
	ra, err := client.ContentStore().ReaderAt(ctx, ocispec.Descriptor{Digest: recordInDgst})
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	return recordInFromReader(ctx, client, io.NewSectionReader(ra, 0, ra.Size()), targetImgRef, platform)
}

func recordInFromReader(ctx gocontext.Context, client *containerd.Client, r io.Reader, targetImgRef string, platform platforms.MatchComparer) (map[digest.Digest][]estargz.Option, error) {
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
	logrus.Infof("analyzing blobs of %q", targetImgRef)
	recordOuts, err := imgrecorder.PrioritizedFilesFromPaths(ctx, client, paths, targetImgRef, platform)
	if err != nil {
		return nil, err
	}
	return pathsToOptions(recordOuts), nil
}

func copyRecordFromImage(ctx gocontext.Context, client *containerd.Client, recordInImgRef, targetImgRef string, p platforms.MatchComparer) (map[digest.Digest][]estargz.Option, error) {
	recordOuts, err := imgrecorder.CopyRecordFromImage(ctx, client, recordInImgRef, targetImgRef, p)
	if err != nil {
		return nil, err
	}
	return pathsToOptions(recordOuts), nil
}

func pathsToOptions(paths map[digest.Digest][]string) map[digest.Digest][]estargz.Option {
	layerOpts := make(map[digest.Digest][]estargz.Option)
	for layerDgst, o := range paths {
		layerOpts[layerDgst] = []estargz.Option{estargz.WithPrioritizedFiles(o)}
	}
	return layerOpts
}
