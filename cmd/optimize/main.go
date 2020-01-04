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

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"syscall"

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
	"github.com/ktock/remote-snapshotter/cmd/optimize/logger"
	"github.com/ktock/remote-snapshotter/cmd/optimize/sampler"
	"github.com/ktock/remote-snapshotter/cmd/optimize/sorter"
	imgpkg "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var (
	insecure   = flag.Bool("insecure", false, "allow HTTP connections to the registry which has the prefix \"http://\"")
	noopt      = flag.Bool("noopt", false, "only stargzify and do not optimize layers")
	period     = flag.Int("period", 10, "time period to monitor access log")
	username   = flag.String("user", "", "user name to override image's default config")
	cwd        = flag.String("cwd", "", "working dir to override image's default config")
	cArgs      = flag.String("args", "", "command arguments to override image's default config(in JSON array)")
	entrypoint = flag.String("entrypoint", "", "entrypoint to override image's default config(in JSON array)")
	terminal   = flag.Bool("t", false, "enable terminal for sample container")
	cEnvs      envs
)

type envs struct {
	list []string
}

func (e *envs) String() string {
	return strings.Join(e.list, ", ")
}

func (e *envs) Set(value string) error {
	e.list = append(e.list, value)
	return nil
}

func main() {

	// Set up logs package to get useful messages i.e. progress.
	logs.Warn.SetOutput(os.Stderr)
	logs.Progress.SetOutput(os.Stderr)

	// Parse arguments
	flag.Var(&cEnvs, "env", "environment valulable to add or override to the image's default config")
	flag.Parse()
	if flag.NArg() < 2 {
		fmt.Printf("usage: %s [OPTION]... INPUT_IMAGE OUTPUT_IMAGE\n", os.Args[0])
		os.Exit(1)
	}
	args := flag.Args()
	src, dst, opts, err := parseArgs(args)
	if err != nil {
		log.Fatal(err)
	}

	// Convert and push image
	srcRef, err := parseReference(src)
	if err != nil {
		log.Fatal(err)
	}
	dstRef, err := parseReference(dst)
	if err != nil {
		log.Fatal(err)
	}
	err = convert(srcRef, dstRef, opts...)
	if err != nil {
		log.Fatal(err)
	}
}

func parseArgs(args []string) (src string, dst string, opts []sampler.Option, err error) {
	src = args[0]
	dst = args[1]

	if len(cEnvs.list) > 0 {
		opts = append(opts, sampler.WithEnvs(cEnvs.list))
	}
	if *cArgs != "" {
		var as []string
		err = json.Unmarshal([]byte(*cArgs), &as)
		if err != nil {
			return "", "", nil, errors.Wrapf(err, "invalid option \"args\"")
		}
		opts = append(opts, sampler.WithArgs(as))
	}
	if *entrypoint != "" {
		var es []string
		err = json.Unmarshal([]byte(*entrypoint), &es)
		if err != nil {
			return "", "", nil, errors.Wrapf(err, "invalid option \"entrypoint\"")
		}
		opts = append(opts, sampler.WithEntrypoint(es))
	}
	if *username != "" {
		opts = append(opts, sampler.WithUser(*username))
	}
	if *cwd != "" {
		opts = append(opts, sampler.WithWorkingDir(*cwd))
	}
	if *terminal {
		opts = append(opts, sampler.WithTerminal())
	}

	return
}

func parseReference(path string) (name.Reference, error) {
	var opts []name.Option
	if strings.HasPrefix(path, "http://") {
		path = strings.TrimPrefix(path, "http://")
		if *insecure {
			opts = append(opts, name.Insecure)
		} else {
			return nil, fmt.Errorf("\"-insecure\" option must be specified to connect to %q using HTTP", path)
		}
	}
	ref, err := name.ParseReference(path, opts...)
	if err != nil {
		return nil, err
	}

	return ref, nil
}

func convert(srcRef, dstRef name.Reference, runopts ...sampler.Option) error {
	// Pull source image
	srcImg, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return err
	}

	// Optimize the image
	// The order of the list is base layer first, top layer last.
	layers, err := srcImg.Layers()
	if err != nil {
		return err
	}
	if !*noopt {
		configData, err := srcImg.RawConfigFile()
		if err != nil {
			return err
		}
		var config imgpkg.Image
		if err := json.Unmarshal(configData, &config); err != nil {
			return fmt.Errorf("failed to parse image config file: %v", err)
		}
		layers, err = optimize(layers, config, runopts...)
		if err != nil {
			return err
		}
	} else {
		for i, l := range layers {
			r, err := l.Uncompressed()
			if err != nil {
				return err
			}
			layers[i] = &layer{r: r}
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
		return err
	}

	return nil
}

// The order of the "in" list must be base layer first, top layer last.
func optimize(in []regpkg.Layer, config imgpkg.Image, opts ...sampler.Option) (out []regpkg.Layer, err error) {
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
				return errors.Wrapf(err, "failed to mount on %s", mp)
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
	if err = sampler.Run(bundle, config, *period, opts...); err != nil {
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
