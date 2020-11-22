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
	"compress/gzip"
	"context"
	gocontext "context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/logger"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/sampler"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	"github.com/google/crfs/stargz"
	"github.com/google/go-containerregistry/pkg/authn"
	reglogs "github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/hashicorp/go-multierror"
	ocidigest "github.com/opencontainers/go-digest"
	spec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
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
			Name:  "stargz-only",
			Usage: "only stargzify and do not optimize layers",
		},
		cli.BoolFlag{
			Name:  "reuse",
			Usage: "reuse eStargz (already optimized) layers without further conversion",
		},
		cli.BoolFlag{
			Name:  "terminal,t",
			Usage: "enable terminal for sample container",
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
			Name:  "no-optimization",
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

		// Convert and push the image
		srcIndex, err := srcIO.readIndex()
		if err != nil {
			// No index found. Try to deal it as a thin image.
			log.G(ctx).Warn("index not found; treating as a thin image with ignoring the platform option")
			srcImage, err := srcIO.readImage()
			if err != nil {
				return err
			}
			p := platforms.DefaultSpec()
			dstImage, err := convertImage(ctx, context, srcImage, &p, tf, opts...)
			if err != nil {
				return err
			}
			return dstIO.writeImage(dstImage)
		}
		dstIndex, err := convertIndex(ctx, context, srcIndex, platform, tf, opts...)
		if err != nil {
			return err
		}
		return dstIO.writeIndex(dstIndex)
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

func parseReference(ref string, clicontext *cli.Context) (imageIO, error) {
	if strings.HasPrefix(ref, "local://") {
		abspath, err := filepath.Abs(strings.TrimPrefix(ref, "local://"))
		if err != nil {
			return nil, err
		}
		return localImage{abspath}, nil
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
	return remoteImage{remoteRef}, nil
}

func convertIndex(ctx gocontext.Context, clicontext *cli.Context, srcIndex regpkg.ImageIndex, platform *spec.Platform, tf *tempfiles.TempFiles, runopts ...sampler.Option) (regpkg.ImageIndex, error) {
	var addendums []mutate.IndexAddendum
	manifest, err := srcIndex.IndexManifest()
	if err != nil {
		return nil, err
	}
	for _, m := range manifest.Manifests {
		p := platforms.DefaultSpec()
		if m.Platform != nil {
			p = *(specPlatform(m.Platform))
		}
		if platform != nil {
			if !platforms.NewMatcher(*platform).Match(p) {
				continue
			}
		}
		srcImg, err := srcIndex.Image(m.Digest)
		if err != nil {
			return nil, err
		}
		cctx := log.WithLogger(ctx, log.G(ctx).WithField("platform", platforms.Format(p)))
		dstImg, err := convertImage(cctx, clicontext, srcImg, &p, tf, runopts...)
		if err != nil {
			return nil, err
		}
		desc, err := partial.Descriptor(dstImg)
		if err != nil {
			return nil, err
		}
		desc.Platform = m.Platform // inherit the platform information
		addendums = append(addendums, mutate.IndexAddendum{
			Add:        dstImg,
			Descriptor: *desc,
		})
	}
	if len(addendums) == 0 {
		return nil, fmt.Errorf("no target image is specified")
	}

	// Push the converted image
	return mutate.AppendManifests(empty.Index, addendums...), nil
}

func convertImage(ctx gocontext.Context, clicontext *cli.Context, srcImg regpkg.Image, platform *spec.Platform, tf *tempfiles.TempFiles, runopts ...sampler.Option) (dstImg regpkg.Image, _ error) {
	// The order of the list is base layer first, top layer last.
	layers, err := srcImg.Layers()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get image layers")
	}
	addendums := make([]mutate.Addendum, len(layers))
	if clicontext.Bool("stargz-only") {
		// TODO: enable to reuse layers
		var eg errgroup.Group
		var addendumsMu sync.Mutex
		for i, l := range layers {
			i, l := i, l
			eg.Go(func() error {
				newL, err := buildStargzLayer(l, tf)
				if err != nil {
					return err
				}
				addendumsMu.Lock()
				addendums[i] = mutate.Addendum{Layer: newL}
				addendumsMu.Unlock()
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, errors.Wrapf(err, "failed to convert layer to stargz")
		}
	} else if clicontext.Bool("no-optimize") || !platforms.NewMatcher(platforms.DefaultSpec()).Match(*platform) {
		// Do not run the optimization container if the option requires it or
		// the source image doesn't match to the platform where this command runs on.
		log.G(ctx).Warn("Platform mismatch or optimization disabled; converting without optimization")
		// TODO: enable to reuse layers
		var eg errgroup.Group
		var addendumsMu sync.Mutex
		for i, l := range layers {
			i, l := i, l
			eg.Go(func() error {
				newL, jtocDigest, err := buildEStargzLayer(l, tf)
				if err != nil {
					return err
				}
				addendumsMu.Lock()
				addendums[i] = mutate.Addendum{
					Layer: newL,
					Annotations: map[string]string{
						estargz.TOCJSONDigestAnnotation: jtocDigest.String(),
					},
				}
				addendumsMu.Unlock()
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, errors.Wrapf(err, "failed to convert layer to stargz")
		}
	} else {
		addendums, err = optimize(ctx, clicontext, srcImg, tf, runopts...)
		if err != nil {
			return nil, err
		}
	}
	srcCfg, err := srcImg.ConfigFile()
	if err != nil {
		return nil, err
	}
	srcCfg.RootFS.DiffIDs = []regpkg.Hash{}
	srcCfg.History = []regpkg.History{}
	img, err := mutate.ConfigFile(empty.Image, srcCfg)
	if err != nil {
		return nil, err
	}

	return mutate.Append(img, addendums...)
}

func optimize(ctx gocontext.Context, clicontext *cli.Context, srcImg regpkg.Image, tf *tempfiles.TempFiles, opts ...sampler.Option) ([]mutate.Addendum, error) {
	// Get image's basic information
	manifest, err := srcImg.Manifest()
	if err != nil {
		return nil, err
	}
	configData, err := srcImg.RawConfigFile()
	if err != nil {
		return nil, err
	}
	var config spec.Image
	if err := json.Unmarshal(configData, &config); err != nil {
		return nil, errors.Wrap(err, "failed to parse image config file")
	}
	// The order is base layer first, top layer last.
	in, err := srcImg.Layers()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get image layers")
	}

	// Setup temporary workspace
	tmpRoot, err := ioutil.TempDir("", "optimize-work")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpRoot)
	log.G(ctx).Debugf("workspace directory: %q", tmpRoot)
	mktemp := func(name string) (path string, err error) {
		if path, err = ioutil.TempDir(tmpRoot, "optimize-"+name+"-"); err != nil {
			return "", err
		}
		if err = os.Chmod(path, 0755); err != nil {
			return "", err
		}
		return path, nil
	}

	// mount layer loggers on temp directories.
	var (
		eg           errgroup.Group
		lowerdirs    []string
		convertLayer = make([](func() (mutate.Addendum, error)), len(in))
	)
	for i := range in {
		i := i
		dgst, err := in[i].Digest()
		if err != nil {
			return nil, err
		}
		ctx := log.WithLogger(ctx, log.G(ctx).WithField("digest", dgst))
		mp, err := mktemp(fmt.Sprintf("lower%d", i))
		if err != nil {
			return nil, err
		}
		defer syscall.Unmount(mp, syscall.MNT_FORCE)
		lowerdirs = append([]string{mp}, lowerdirs...) // top layer first, base layer last (for overlayfs).
		eg.Go(func() error {
			// TODO: These files should be deduplicated.
			compressedFile, err := tf.TempFile("", "compresseddata")
			if err != nil {
				return err
			}
			decompressedFile, err := tf.TempFile("", "decompresseddata")
			if err != nil {
				return err
			}

			// Mount the layer
			r, err := in[i].Compressed()
			if err != nil {
				return err
			}
			defer r.Close()
			zr, err := gzip.NewReader(io.TeeReader(r, compressedFile))
			if err != nil {
				return err
			}
			defer zr.Close()
			if _, err := io.Copy(decompressedFile, zr); err != nil {
				return err
			}
			mon := logger.NewOpenReadMonitor()
			if _, err := logger.Mount(mp, decompressedFile, mon); err != nil {
				return errors.Wrapf(err, "failed to mount on %q", mp)
			}

			// Prepare converters according to the layer type
			var cvts []func() (mutate.Addendum, error)
			if tocdgst, ok := getTOCDigest(manifest, dgst); ok && clicontext.Bool("reuse") {
				// If this layer is a valid eStargz, try to reuse this layer.
				// If no access occur to this layer during the specified workload,
				// this layer will be reused without conversion.
				compressedLayer, err := fileSectionReader(compressedFile)
				if err != nil {
					return err
				}
				f, err := converterFromEStargz(ctx, tocdgst, in[i], compressedLayer, mon)
				if err == nil {
					// TODO: remotely mount it instead of downloading the layer.
					cvts = append(cvts, f)
				}
			}
			decompressedLayer, err := fileSectionReader(decompressedFile)
			if err != nil {
				return err
			}
			convertLayer[i] = converters(
				append(cvts, converterFromTar(ctx, decompressedLayer, mon, tf))...)
			log.G(ctx).Infof("unpacked")
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// prepare FileSystem Bundle
	var (
		bundle   string
		upperdir string
		workdir  string
	)
	if bundle, err = mktemp("bundle"); err != nil {
		return nil, err
	}
	if upperdir, err = mktemp("upperdir"); err != nil {
		return nil, err
	}
	if workdir, err = mktemp("workdir"); err != nil {
		return nil, err
	}
	var (
		rootfs = sampler.GetRootfsPathUnder(bundle)
		option = fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			strings.Join(lowerdirs, ":"), upperdir, workdir)
	)
	if err = os.Mkdir(rootfs, 0777); err != nil {
		return nil, err
	}
	if err = syscall.Mount("overlay", rootfs, "overlay", 0, option); err != nil {
		return nil, errors.Wrapf(err, "mount overlayfs on %q with data %q", rootfs, option)
	}
	defer syscall.Unmount(rootfs, syscall.MNT_FORCE)

	// run the workload with timeout
	runCtx, cancel := gocontext.WithTimeout(ctx,
		time.Duration(clicontext.Int("period"))*time.Second)
	defer cancel()
	if err = sampler.Run(runCtx, bundle, config, opts...); err != nil {
		return nil, errors.Wrap(err, "failed to run the sampler")
	}

	// get converted layers
	var (
		adds   = make([]mutate.Addendum, len(convertLayer))
		addsMu sync.Mutex
	)
	for i, f := range convertLayer {
		i, f := i, f
		eg.Go(func() error {
			addendum, err := f()
			if err != nil {
				return errors.Wrap(err, "failed to get converted layer")
			}
			addsMu.Lock()
			adds[i] = addendum
			addsMu.Unlock()

			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return adds, nil
}

func converterFromTar(ctx context.Context, sr *io.SectionReader, mon logger.Monitor, tf *tempfiles.TempFiles) func() (mutate.Addendum, error) {
	return func() (mutate.Addendum, error) {
		log.G(ctx).Debugf("converting...")
		defer log.G(ctx).Infof("converted")

		rc, jtocDigest, err := estargz.Build(sr, mon.DumpLog())
		if err != nil {
			return mutate.Addendum{}, err
		}
		defer rc.Close()
		log.G(ctx).WithField("TOC JSON digest", jtocDigest).Debugf("calculated digest")
		l, err := newStaticCompressedLayer(rc, tf)
		if err != nil {
			return mutate.Addendum{}, err
		}
		return mutate.Addendum{
			Layer: l,
			Annotations: map[string]string{
				estargz.TOCJSONDigestAnnotation: jtocDigest.String(),
			},
		}, nil
	}
}

func converterFromEStargz(ctx gocontext.Context, tocdgst ocidigest.Digest, l regpkg.Layer, sr *io.SectionReader, mon logger.Monitor) (func() (mutate.Addendum, error), error) {
	// If the layer is valid eStargz, use this layer without conversion
	if _, err := stargz.Open(sr); err != nil {
		return nil, err
	}
	if _, err := estargz.VerifyStargzTOC(sr, tocdgst); err != nil {
		return nil, err
	}
	dgst, err := l.Digest()
	if err != nil {
		return nil, err
	}
	diff, err := l.DiffID()
	if err != nil {
		return nil, err
	}
	return func() (mutate.Addendum, error) {
		if len(mon.DumpLog()) != 0 {
			// There have been some accesses to this layer. we don't reuse this.
			return mutate.Addendum{}, fmt.Errorf("unable to reuse accessed layer")
		}
		log.G(ctx).Infof("no access occur; copying without conversion")
		return mutate.Addendum{
			Layer: staticCompressedLayer{
				r:    sr,
				diff: diff,
				hash: dgst,
				size: sr.Size(),
			},
			Annotations: map[string]string{
				estargz.TOCJSONDigestAnnotation: tocdgst.String(),
			},
		}, nil
	}, nil
}

func converters(cs ...func() (mutate.Addendum, error)) func() (mutate.Addendum, error) {
	return func() (add mutate.Addendum, allErr error) {
		for _, f := range cs {
			a, err := f()
			if err == nil {
				return a, nil
			}
			allErr = multierror.Append(allErr, err)
		}
		return
	}
}

func buildStargzLayer(uncompressed regpkg.Layer, tf *tempfiles.TempFiles) (regpkg.Layer, error) {
	r, err := uncompressed.Uncompressed()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		w := stargz.NewWriter(pw)
		if err := w.AppendTar(r); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := w.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	return newStaticCompressedLayer(pr, tf)
}

func buildEStargzLayer(uncompressed regpkg.Layer, tf *tempfiles.TempFiles) (regpkg.Layer, ocidigest.Digest, error) {
	tftmp := tempfiles.NewTempFiles() // Shorter lifetime than tempfiles passed by argument
	defer tftmp.CleanupAll()
	r, err := uncompressed.Uncompressed()
	if err != nil {
		return nil, "", err
	}
	file, err := tftmp.TempFile("", "tmpdata")
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(file, r); err != nil {
		return nil, "", err
	}
	sr, err := fileSectionReader(file)
	if err != nil {
		return nil, "", err
	}
	rc, jtocDigest, err := estargz.Build(sr, nil) // no optimization
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	l, err := newStaticCompressedLayer(rc, tf)
	if err != nil {
		return nil, "", err
	}
	return l, jtocDigest, err
}

func newStaticCompressedLayer(compressed io.Reader, tf *tempfiles.TempFiles) (regpkg.Layer, error) {
	file, err := tf.TempFile("", "layerdata")
	if err != nil {
		return nil, err
	}
	var (
		diff = sha256.New()
		h    = sha256.New()
	)
	zr, err := gzip.NewReader(io.TeeReader(compressed, io.MultiWriter(file, h)))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	if _, err := io.Copy(diff, zr); err != nil {
		return nil, err
	}
	sr, err := fileSectionReader(file)
	if err != nil {
		return nil, err
	}
	return staticCompressedLayer{
		r: sr,
		diff: regpkg.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(diff.Sum(nil)),
		},
		hash: regpkg.Hash{
			Algorithm: "sha256",
			Hex:       hex.EncodeToString(h.Sum(nil)),
		},
		size: sr.Size(),
	}, nil
}

type staticCompressedLayer struct {
	r    io.Reader
	diff regpkg.Hash
	hash regpkg.Hash
	size int64
}

func (l staticCompressedLayer) Digest() (regpkg.Hash, error) {
	return l.hash, nil
}

func (l staticCompressedLayer) Size() (int64, error) {
	return l.size, nil
}

func (l staticCompressedLayer) DiffID() (regpkg.Hash, error) {
	return l.diff, nil
}

func (l staticCompressedLayer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

func (l staticCompressedLayer) Compressed() (io.ReadCloser, error) {
	// TODO: We should pass l.closerFunc to ggcr as Close() of io.ReadCloser
	//       but ggcr currently doesn't call Close() so we close it manually on EOF.
	//       See also: https://github.com/google/go-containerregistry/pull/768
	return ioutil.NopCloser(l.r), nil
}

func (l staticCompressedLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("unsupported")
}

// imageIO is an interface for helpers of reading/writing images to/from somewhere.
type imageIO interface {
	readIndex() (regpkg.ImageIndex, error)
	writeIndex(index regpkg.ImageIndex) error
	readImage() (regpkg.Image, error)
	writeImage(image regpkg.Image) error
}

// remoteImage is a helper for reading/writing images stored in the remote registry.
type remoteImage struct {
	remoteRef name.Reference
}

func (ri remoteImage) readIndex() (regpkg.ImageIndex, error) {
	return remote.Index(ri.remoteRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

func (ri remoteImage) writeIndex(index regpkg.ImageIndex) error {
	return remote.WriteIndex(ri.remoteRef, index, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

func (ri remoteImage) readImage() (regpkg.Image, error) {
	desc, err := remote.Get(ri.remoteRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, err
	}
	return desc.Image()
}

func (ri remoteImage) writeImage(image regpkg.Image) error {
	return remote.Write(ri.remoteRef, image, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

// localImage is a helper for reading/writing images stored in the OCI Image Layout directory.
type localImage struct {
	localPath string
}

func (li localImage) readIndex() (regpkg.ImageIndex, error) {
	lp, err := layout.FromPath(li.localPath)
	if err != nil {
		return nil, err
	}
	return lp.ImageIndex()
}

func (li localImage) writeIndex(index regpkg.ImageIndex) error {
	_, err := layout.Write(li.localPath, index)
	return err
}

func (li localImage) readImage() (regpkg.Image, error) {
	// OCI Image Layout doesn't have representation of thin image
	return nil, fmt.Errorf("thin image cannot be read from local")
}

func (li localImage) writeImage(image regpkg.Image) error {
	// OCI layout requires index so create it.
	// TODO: Should we add platform information here?
	desc, err := partial.Descriptor(image)
	if err != nil {
		return err
	}
	_, err = layout.Write(li.localPath, mutate.AppendManifests(
		empty.Index,
		mutate.IndexAddendum{
			Add:        image,
			Descriptor: *desc,
		},
	))
	return err
}

func getTOCDigest(manifest *regpkg.Manifest, dgst regpkg.Hash) (ocidigest.Digest, bool) {
	if manifest == nil {
		return "", false
	}
	for _, desc := range manifest.Layers {
		if desc.Digest.Algorithm == dgst.Algorithm && desc.Digest.Hex == dgst.Hex {
			dgstStr, ok := desc.Annotations[estargz.TOCJSONDigestAnnotation]
			if ok {
				if tocdgst, err := ocidigest.Parse(dgstStr); err == nil {
					return tocdgst, true
				}
			}
		}
	}
	return "", false
}

func fileSectionReader(file *os.File) (*io.SectionReader, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return io.NewSectionReader(file, 0, info.Size()), nil
}

// specPlatform converts ggcr's platform struct to OCI's struct
func specPlatform(p *regpkg.Platform) *spec.Platform {
	return &spec.Platform{
		Architecture: p.Architecture,
		OS:           p.OS,
		OSVersion:    p.OSVersion,
		OSFeatures:   p.OSFeatures,
		Variant:      p.Variant,
	}
}
