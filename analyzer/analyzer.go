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

package analyzer

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/stargz-snapshotter/analyzer/logger"
	"github.com/containerd/stargz-snapshotter/analyzer/sampler"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var defaultPeriod = 10 * time.Second

type analyzerOpts struct {
	samplerOpts []sampler.Option
	period      time.Duration
}

type Option func(opts *analyzerOpts)

// WithSamplerOpts is options for sampler
func WithSamplerOpts(samplerOpts ...sampler.Option) Option {
	return func(opts *analyzerOpts) {
		opts.samplerOpts = samplerOpts
	}
}

// WithPeriod is the period to run sampler
func WithPeriod(period time.Duration) Option {
	return func(opts *analyzerOpts) {
		opts.period = period
	}
}

// Analyze analyzes the passed uncompressed layers and configuration then
// returns prioritized files per layer.
func Analyze(in []io.ReaderAt, imageConfig ocispec.Image, opts ...Option) ([][]string, error) {
	var aOpts analyzerOpts
	for _, o := range opts {
		o(&aOpts)
	}

	// Setup temporary workspace
	tmpRoot, err := ioutil.TempDir("", "optimize-work")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpRoot)
	mktemp := func(name string) (path string, err error) {
		if path, err = ioutil.TempDir(tmpRoot, "optimize-"+name+"-"); err != nil {
			return "", err
		}
		if err = os.Chmod(path, 0755); err != nil {
			return "", err
		}
		return path, nil
	}

	var (
		eg        errgroup.Group
		lowerdirs []string
	)
	monitors := make([]logger.Monitor, len(in))

	// mount layer loggers on temp directories.
	for i := range in {
		i := i
		mon := logger.NewOpenReadMonitor()
		monitors[i] = mon
		mp, err := mktemp(fmt.Sprintf("lower%d", i))
		if err != nil {
			return nil, err
		}
		defer syscall.Unmount(mp, syscall.MNT_FORCE)
		lowerdirs = append(lowerdirs, mp)
		eg.Go(func() error {
			if _, err := logger.Mount(mp, in[i], mon); err != nil {
				return errors.Wrapf(err, "failed to mount on %q", mp)
			}
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
	period := aOpts.period
	if period == 0 {
		period = defaultPeriod
	}
	runCtx, cancel := context.WithTimeout(context.Background(), period)
	defer cancel()
	if err = sampler.Run(runCtx, bundle, imageConfig, aOpts.samplerOpts...); err != nil {
		return nil, errors.Wrap(err, "failed to run the sampler")
	}

	// harvest the result log
	results := make([][]string, len(monitors))
	for i, mon := range monitors {
		results[i] = mon.DumpLog()
	}
	return results, nil
}
