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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"syscall"

	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/logger"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/sampler"
	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/sorter"
	"github.com/google/crfs/stargz"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/stream"
	"github.com/google/go-containerregistry/pkg/v1/types"
	imgpkg "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
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
			Name:  "t",
			Usage: "only stargzify and do not optimize layers",
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
	},
	Action: func(context *cli.Context) error {

		// Set up logs package to get useful messages e.g. progress.
		logs.Warn.SetOutput(os.Stderr)
		logs.Progress.SetOutput(os.Stderr)

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
		err = convert(context, srcRef, dstRef, opts...)
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
	if clicontext.Bool("t") {
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

func convert(clicontext *cli.Context, srcRef, dstRef name.Reference, runopts ...sampler.Option) error {
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
	if clicontext.Bool("stargz-only") {
		for i, l := range layers {
			r, err := l.Uncompressed()
			if err != nil {
				return errors.Wrapf(err, "failed to convert layer(%d) to stargz", i)
			}
			layers[i] = &layer{r: r}
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
		layers, err = optimize(clicontext, layers, config, runopts...)
		if err != nil {
			return err
		}
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
	img, err = mutate.AppendLayers(img, layers...)
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
func optimize(clicontext *cli.Context, in []regpkg.Layer, config imgpkg.Image, opts ...sampler.Option) (out []regpkg.Layer, err error) {
	root := ""
	mktemp := func() (path string, err error) {
		if path, err = ioutil.TempDir(root, "optimize"); err != nil {
			return "", err
		}
		if err = os.Chmod(path, 0777); err != nil {
			return "", err
		}
		return path, nil
	}
	if root, err = mktemp(); err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)

	// mount layer loggers on temp directories.
	var (
		eg        errgroup.Group
		lowerdirs []string
		addLayer  = make([](func([]regpkg.Layer) ([]regpkg.Layer, error)), len(in))
	)
	for i := range in {
		i := i
		mp, err := mktemp()
		if err != nil {
			return nil, err
		}
		defer syscall.Unmount(mp, syscall.MNT_FORCE)
		lowerdirs = append([]string{mp}, lowerdirs...) // top layer first, base layer last (for overlayfs).
		eg.Go(func() error {
			dgst, err := in[i].Digest()
			if err != nil {
				return err
			}
			r, err := in[i].Uncompressed()
			if err != nil {
				return err
			}
			defer r.Close()
			tarBytes, err := ioutil.ReadAll(r)
			if err != nil {
				return err
			}
			mon := logger.NewOpenReadMonitor()
			if _, err := logger.Mount(mp, bytes.NewReader(tarBytes), mon); err != nil {
				return errors.Wrapf(err, "failed to mount on %q", mp)
			}
			addLayer[i] = func(lowers []regpkg.Layer) ([]regpkg.Layer, error) {
				r, err := sorter.Sort(bytes.NewReader(tarBytes), mon.DumpLog())
				if err != nil {
					return nil, errors.Wrap(err, "failed to sort tar")
				}
				return append(lowers, &layer{r: r}), nil
			}
			fmt.Printf("Unpacked %v\n", dgst)
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
	if bundle, err = mktemp(); err != nil {
		return nil, err
	}
	if upperdir, err = mktemp(); err != nil {
		return nil, err
	}
	if workdir, err = mktemp(); err != nil {
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

	// run workload
	if err = sampler.Run(bundle, config, clicontext.Int("period"), opts...); err != nil {
		return nil, errors.Wrap(err, "failed to run the sampler")
	}

	// get converted layers
	for _, f := range addLayer {
		if out, err = f(out); err != nil {
			return nil, errors.Wrap(err, "failed to get converted layer")
		}
	}
	return
}

type writer func([]byte) (int, error)

func (f writer) Write(b []byte) (int, error) { return f(b) }

type layer struct {
	r    io.Reader
	diff *regpkg.Hash // registered after compression completed
	hash *hash.Hash   // registered after compression completed
	size *int64       // registered after compression completed
}

func (l *layer) Digest() (regpkg.Hash, error) {
	if l.hash == nil {
		return regpkg.Hash{}, stream.ErrNotComputed
	}
	return regpkg.Hash{
		Algorithm: "sha256",
		Hex:       hex.EncodeToString((*l.hash).Sum(nil)),
	}, nil
}

func (l *layer) Size() (int64, error) {
	if l.size == nil {
		return -1, stream.ErrNotComputed
	}
	return *l.size, nil
}

func (l *layer) DiffID() (regpkg.Hash, error) {
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
		l.diff = &diff
		l.hash = &h
		l.size = &size
		pw.Close()
	}()
	return ioutil.NopCloser(pr), nil
}

func (l *layer) Uncompressed() (io.ReadCloser, error) {
	return ioutil.NopCloser(l.r), nil
}
