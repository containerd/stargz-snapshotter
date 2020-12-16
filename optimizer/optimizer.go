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

package optimizer

import (
	"compress/gzip"
	gocontext "context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/optimizer/converter"
	"github.com/containerd/stargz-snapshotter/optimizer/layer"
	"github.com/containerd/stargz-snapshotter/optimizer/logger"
	"github.com/containerd/stargz-snapshotter/optimizer/sampler"
	"github.com/containerd/stargz-snapshotter/optimizer/util"
	"github.com/containerd/stargz-snapshotter/util/tempfiles"
	regpkg "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	ocidigest "github.com/opencontainers/go-digest"
	spec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type Opts struct {
	NoOptimize bool
	Reuse      bool
	Period     time.Duration
}

func ConvertIndex(ctx gocontext.Context, opts *Opts, srcIndex regpkg.ImageIndex, platform *spec.Platform, tf *tempfiles.TempFiles, runopts ...sampler.Option) (regpkg.ImageIndex, error) {
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
		dstImg, err := ConvertImage(cctx, opts, srcImg, &p, tf, runopts...)
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

func ConvertImage(ctx gocontext.Context, opts *Opts, srcImg regpkg.Image, platform *spec.Platform, tf *tempfiles.TempFiles, runopts ...sampler.Option) (dstImg regpkg.Image, _ error) {
	// The order of the list is base layer first, top layer last.
	layers, err := srcImg.Layers()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get image layers")
	}
	addendums := make([]mutate.Addendum, len(layers))
	if opts.NoOptimize || !platforms.NewMatcher(platforms.DefaultSpec()).Match(*platform) {
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
		addendums, err = Optimize(ctx, opts, srcImg, tf, runopts...)
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

func Optimize(ctx gocontext.Context, opts *Opts, srcImg regpkg.Image, tf *tempfiles.TempFiles, samplerOpts ...sampler.Option) ([]mutate.Addendum, error) {
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
			if tocdgst, ok := getTOCDigest(manifest, dgst); ok && opts.Reuse {
				// If this layer is a valid eStargz, try to reuse this layer.
				// If no access occur to this layer during the specified workload,
				// this layer will be reused without conversion.
				compressedLayer, err := util.FileSectionReader(compressedFile)
				if err != nil {
					return err
				}
				f, err := converter.FromEStargz(ctx, tocdgst, in[i], compressedLayer, mon)
				if err == nil {
					// TODO: remotely mount it instead of downloading the layer.
					cvts = append(cvts, f)
				}
			}
			decompressedLayer, err := util.FileSectionReader(decompressedFile)
			if err != nil {
				return err
			}
			convertLayer[i] = converter.Converters(
				append(cvts, converter.FromTar(ctx, decompressedLayer, mon, tf))...)
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
	runCtx, cancel := gocontext.WithTimeout(ctx, opts.Period)
	defer cancel()
	if err = sampler.Run(runCtx, bundle, config, samplerOpts...); err != nil {
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
	sr, err := util.FileSectionReader(file)
	if err != nil {
		return nil, "", err
	}
	rc, jtocDigest, err := estargz.Build(sr, nil) // no optimization
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	l, err := layer.NewStaticCompressedLayer(rc, tf)
	if err != nil {
		return nil, "", err
	}
	return l, jtocDigest, err
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
