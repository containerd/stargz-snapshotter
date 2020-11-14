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
	gocontext "context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/builder"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/logger"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/sampler"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/sorter"
	"github.com/containerd/stargz-snapshotter/stargz/verify"
	"github.com/google/crfs/stargz"
	"github.com/google/go-containerregistry/pkg/authn"
	reglogs "github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/stream"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/hashicorp/go-multierror"
	ocidigest "github.com/opencontainers/go-digest"
	imgpkg "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

const defaultPeriod = 10

var OptimizeCommand = cli.Command{
	Name:      "optimize",
	Usage:     "optimize an image with user-specified workload",
	ArgsUsage: "[flags] <input-ref> <output-ref>",
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

		// Convert and push image
		srcRef, err := parseReference(src, context)
		if err != nil {
			return errors.Wrapf(err, "failed to parse source ref %q", src)
		}
		dstRef, err := parseReference(dst, context)
		if err != nil {
			return errors.Wrapf(err, "failed to parse destination ref %q", dst)
		}
		err = convert(ctx, context, srcRef, dstRef, opts...)
		if err != nil {
			return errors.Wrapf(err, "failed to convert image %q -> %q",
				srcRef.String(), dstRef.String())
		}
		return nil
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

	return
}

func parseReference(path string, clicontext *cli.Context) (name.Reference, error) {
	var opts []name.Option
	if strings.HasPrefix(path, "http://") {
		path = strings.TrimPrefix(path, "http://")
		if clicontext.Bool("plain-http") {
			opts = append(opts, name.Insecure)
		} else {
			return nil, fmt.Errorf("\"--plain-http\" option must be specified to connect to %q using HTTP", path)
		}
	}
	ref, err := name.ParseReference(path, opts...)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse reference %q", path)
	}

	return ref, nil
}

func convert(ctx gocontext.Context, clicontext *cli.Context, srcRef, dstRef name.Reference, runopts ...sampler.Option) error {
	// Pull source image
	srcImg, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return errors.Wrapf(err, "failed to pull image from %q", srcRef.String())
	}

	// Optimize the image
	// The order of the list is base layer first, top layer last.
	layers, err := srcImg.Layers()
	if err != nil {
		return errors.Wrap(err, "failed to get image layers")
	}
	addendums := make([]mutate.Addendum, len(layers))
	if clicontext.Bool("stargz-only") {
		for i, l := range layers {
			r, err := l.Uncompressed()
			if err != nil {
				return errors.Wrapf(err, "failed to convert layer(%d) to stargz", i)
			}
			addendums[i] = mutate.Addendum{Layer: &layer{r: r}}
		}
	} else {
		configData, err := srcImg.RawConfigFile()
		if err != nil {
			return err
		}
		var config imgpkg.Image
		if err := json.Unmarshal(configData, &config); err != nil {
			return errors.Wrap(err, "failed to parse image config file")
		}
		manifest, err := srcImg.Manifest()
		if err != nil {
			return err
		}
		var done func()
		addendums, done, err = optimize(ctx, clicontext, layers, manifest, config, runopts...)
		if err != nil {
			return err
		}
		defer done()
	}
	srcCfg, err := srcImg.ConfigFile()
	if err != nil {
		return err
	}
	srcCfg.RootFS.DiffIDs = []regpkg.Hash{}
	srcCfg.History = []regpkg.History{}
	img, err := mutate.ConfigFile(empty.Image, srcCfg)
	if err != nil {
		return err
	}
	img, err = mutate.Append(img, addendums...)
	if err != nil {
		return err
	}

	// Push the optimized image.
	if err := remote.Write(dstRef, img, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		return errors.Wrapf(err, "failed to push image to %q", dstRef.String())
	}

	return nil
}

// The order of the "in" list must be base layer first, top layer last.
func optimize(ctx gocontext.Context, clicontext *cli.Context, in []regpkg.Layer, manifest *regpkg.Manifest, config imgpkg.Image, opts ...sampler.Option) (out []mutate.Addendum, done func(), err error) {
	// Setup temporary workspace
	tmpRoot, err := ioutil.TempDir("", "optimize-work")
	if err != nil {
		return nil, nil, err
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
		layerFiles   []*os.File
		layerFilesMu sync.Mutex
	)
	done = func() {
		for _, f := range layerFiles {
			if err := f.Close(); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to close tmpfile %v", f.Name())
			}
			if err := os.Remove(f.Name()); err != nil {
				log.G(ctx).WithError(err).Warnf("failed to remove tmpfile %v", f.Name())
			}
		}
	}
	for i := range in {
		i := i
		dgst, err := in[i].Digest()
		if err != nil {
			return nil, nil, err
		}
		ctx := log.WithLogger(ctx, log.G(ctx).WithField("digest", dgst))
		mp, err := mktemp(fmt.Sprintf("lower%d", i))
		if err != nil {
			return nil, nil, err
		}
		defer syscall.Unmount(mp, syscall.MNT_FORCE)
		lowerdirs = append([]string{mp}, lowerdirs...) // top layer first, base layer last (for overlayfs).
		eg.Go(func() error {
			// TODO: These files should be deduplicated.
			compressedFile, err := ioutil.TempFile("", "compresseddata")
			if err != nil {
				return err
			}
			decompressedFile, err := ioutil.TempFile("", "decompresseddata")
			if err != nil {
				return err
			}
			stargzFile, err := ioutil.TempFile("", "stargzdata")
			if err != nil {
				return err
			}
			layerFilesMu.Lock()
			layerFiles = append(layerFiles, compressedFile, decompressedFile, stargzFile)
			layerFilesMu.Unlock()

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
			cinfo, err := compressedFile.Stat()
			if err != nil {
				return err
			}
			dinfo, err := decompressedFile.Stat()
			if err != nil {
				return err
			}
			compressedLayer := io.NewSectionReader(compressedFile, 0, cinfo.Size())
			decompressedLayer := io.NewSectionReader(decompressedFile, 0, dinfo.Size())
			ctar := func() (mutate.Addendum, error) {
				log.G(ctx).Debugf("converting...")
				defer log.G(ctx).Infof("converted")

				// Sort file entry by the accessed order
				entries, err := sorter.Sort(decompressedLayer, mon.DumpLog())
				if err != nil {
					return mutate.Addendum{}, errors.Wrap(err, "failed to sort tar")
				}

				// Build stargz
				if err := builder.BuildStargz(ctx, entries, stargzFile); err != nil {
					return mutate.Addendum{}, err
				}

				// Add chunks digests to TOC JSON
				sinfo, err := stargzFile.Stat()
				if err != nil {
					return mutate.Addendum{}, err
				}
				r, jtocDigest, err := verify.NewVerifiableStagz(
					io.NewSectionReader(stargzFile, 0, sinfo.Size()))
				if err != nil {
					return mutate.Addendum{}, err
				}
				log.G(ctx).WithField("TOC JSON digest", jtocDigest).
					Debugf("calculated digest")

				return mutate.Addendum{
					Layer: &gzipLayer{r: r},
					Annotations: map[string]string{
						verify.TOCJSONDigestAnnotation: jtocDigest.String(),
					},
				}, nil
			}
			var cvts []func() (mutate.Addendum, error)
			if tocdgst, ok := getTOCDigest(manifest, dgst); ok && clicontext.Bool("reuse") {
				// If this layer is a valid eStargz, try to reuse this layer.
				// If no access occur to this layer during the specified workload,
				// this layer will be reused without conversion.
				f, err := converterFromEStargz(ctx, tocdgst, in[i], compressedLayer, mon)
				if err == nil {
					// TODO: remotely mount it instead of downloading the layer.
					cvts = append(cvts, f)
				}
			}
			convertLayer[i] = converters(append(cvts, ctar)...)
			log.G(ctx).Infof("unpacked")
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, nil, err
	}

	// prepare FileSystem Bundle
	var (
		bundle   string
		upperdir string
		workdir  string
	)
	if bundle, err = mktemp("bundle"); err != nil {
		return nil, nil, err
	}
	if upperdir, err = mktemp("upperdir"); err != nil {
		return nil, nil, err
	}
	if workdir, err = mktemp("workdir"); err != nil {
		return nil, nil, err
	}
	var (
		rootfs = sampler.GetRootfsPathUnder(bundle)
		option = fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
			strings.Join(lowerdirs, ":"), upperdir, workdir)
	)
	if err = os.Mkdir(rootfs, 0777); err != nil {
		return nil, nil, err
	}
	if err = syscall.Mount("overlay", rootfs, "overlay", 0, option); err != nil {
		return nil, nil, errors.Wrapf(err, "mount overlayfs on %q with data %q", rootfs, option)
	}
	defer syscall.Unmount(rootfs, syscall.MNT_FORCE)

	// run the workload with timeout
	runCtx, cancel := gocontext.WithTimeout(ctx,
		time.Duration(clicontext.Int("period"))*time.Second)
	defer cancel()
	if err = sampler.Run(runCtx, bundle, config, opts...); err != nil {
		return nil, nil, errors.Wrap(err, "failed to run the sampler")
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
		return nil, nil, err
	}
	out = adds

	return
}

func converterFromEStargz(ctx gocontext.Context, tocdgst ocidigest.Digest, l regpkg.Layer, sr *io.SectionReader, mon logger.Monitor) (func() (mutate.Addendum, error), error) {
	// If the layer is valid eStargz, use this layer without conversion
	if _, err := stargz.Open(sr); err != nil {
		return nil, err
	}
	if _, err := verify.StargzTOC(sr, tocdgst); err != nil {
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
			Layer: &staticCompressedLayer{
				r:    sr,
				diff: diff,
				hash: dgst,
				size: sr.Size(),
			},
			Annotations: map[string]string{
				verify.TOCJSONDigestAnnotation: tocdgst.String(),
			},
		}, nil
	}, nil
}

func getTOCDigest(manifest *regpkg.Manifest, dgst regpkg.Hash) (ocidigest.Digest, bool) {
	if manifest == nil {
		return "", false
	}
	for _, desc := range manifest.Layers {
		if desc.Digest.Algorithm == dgst.Algorithm && desc.Digest.Hex == dgst.Hex {
			dgstStr, ok := desc.Annotations[verify.TOCJSONDigestAnnotation]
			if ok {
				if tocdgst, err := ocidigest.Parse(dgstStr); err == nil {
					return tocdgst, true
				}
			}
		}
	}
	return "", false
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

type writer func([]byte) (int, error)

func (f writer) Write(b []byte) (int, error) { return f(b) }

type layer struct {
	r    io.Reader
	diff *regpkg.Hash // registered after compression completed
	hash *hash.Hash   // registered after compression completed
	size *int64       // registered after compression completed
	mu   sync.Mutex
}

func (l *layer) Digest() (regpkg.Hash, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.hash == nil {
		return regpkg.Hash{}, stream.ErrNotComputed
	}
	return regpkg.Hash{
		Algorithm: "sha256",
		Hex:       hex.EncodeToString((*l.hash).Sum(nil)),
	}, nil
}

func (l *layer) Size() (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.size == nil {
		return -1, stream.ErrNotComputed
	}
	return *l.size, nil
}

func (l *layer) DiffID() (regpkg.Hash, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.diff == nil {
		return regpkg.Hash{}, stream.ErrNotComputed
	}
	return *l.diff, nil
}

func (l *layer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

func (l *layer) Compressed() (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		var (
			diff regpkg.Hash
			h    = sha256.New()
			size int64
			err  error
		)
		w := stargz.NewWriter(io.MultiWriter(pw, writer(func(b []byte) (int, error) {
			n, err := h.Write(b)
			size += int64(n)
			return n, err
		})))
		if err := w.AppendTar(l.r); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := w.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		diff, err = regpkg.NewHash(w.DiffID())
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		// registers all computed information
		l.mu.Lock()
		l.diff = &diff
		l.hash = &h
		l.size = &size
		l.mu.Unlock()

		pw.Close()
	}()
	return ioutil.NopCloser(pr), nil
}

func (l *layer) Uncompressed() (io.ReadCloser, error) {
	return ioutil.NopCloser(l.r), nil
}

type gzipLayer struct {
	r    io.Reader
	diff *hash.Hash // registered after computation completed
	hash *hash.Hash // registered after computation completed
	size *int64     // registered after computation completed
	mu   sync.Mutex
}

func (l *gzipLayer) Digest() (regpkg.Hash, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.hash == nil {
		return regpkg.Hash{}, stream.ErrNotComputed
	}
	return regpkg.Hash{
		Algorithm: "sha256",
		Hex:       hex.EncodeToString((*l.hash).Sum(nil)),
	}, nil
}

func (l *gzipLayer) Size() (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.size == nil {
		return -1, stream.ErrNotComputed
	}
	return *l.size, nil
}

func (l *gzipLayer) DiffID() (regpkg.Hash, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.diff == nil {
		return regpkg.Hash{}, stream.ErrNotComputed
	}
	return regpkg.Hash{
		Algorithm: "sha256",
		Hex:       hex.EncodeToString((*l.diff).Sum(nil)),
	}, nil
}

func (l *gzipLayer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

func (l *gzipLayer) Compressed() (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		var (
			diff = sha256.New()
			h    = sha256.New()
			size int64
		)
		zr, err := gzip.NewReader(io.TeeReader(
			l.r,
			io.MultiWriter(pw, writer(func(b []byte) (int, error) {
				n, err := h.Write(b)
				size += int64(n)
				return n, err
			})),
		))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		defer zr.Close()
		if _, err := io.Copy(diff, zr); err != nil {
			pw.CloseWithError(err)
			return
		}

		// registers all computed information
		l.mu.Lock()
		l.diff = &diff
		l.hash = &h
		l.size = &size
		l.mu.Unlock()

		pw.Close()
	}()
	return ioutil.NopCloser(pr), nil
}

func (l *gzipLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("unsupported")
}

type staticCompressedLayer struct {
	r    io.Reader
	diff regpkg.Hash
	hash regpkg.Hash
	size int64
}

func (l *staticCompressedLayer) Digest() (regpkg.Hash, error) {
	return l.hash, nil
}

func (l *staticCompressedLayer) Size() (int64, error) {
	return l.size, nil
}

func (l *staticCompressedLayer) DiffID() (regpkg.Hash, error) {
	return l.diff, nil
}

func (l *staticCompressedLayer) MediaType() (types.MediaType, error) {
	return types.DockerLayer, nil
}

func (l *staticCompressedLayer) Compressed() (io.ReadCloser, error) {
	return ioutil.NopCloser(l.r), nil
}

func (l *staticCompressedLayer) Uncompressed() (io.ReadCloser, error) {
	return nil, errors.New("unsupported")
}
