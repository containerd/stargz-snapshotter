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

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/fs/source"
	"github.com/containerd/stargz-snapshotter/util/cacheutil"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	refCacheEntry            = 30
	defaultManifestCacheTime = 120 * time.Second
)

func newRefPool(ctx context.Context, root string, hosts source.RegistryHosts) (*refPool, error) {
	var poolroot = filepath.Join(root, "pool")
	if err := os.MkdirAll(poolroot, 0700); err != nil {
		return nil, err
	}
	p := &refPool{
		path:       poolroot,
		hosts:      hosts,
		refcounter: make(map[string]*releaser),
	}
	p.cache = cacheutil.NewLRUCache(refCacheEntry)
	p.cache.OnEvicted = func(key string, value interface{}) {
		refspec := value.(reference.Spec)
		if err := os.RemoveAll(p.metadataDir(refspec)); err != nil {
			log.G(ctx).WithField("key", key).WithError(err).Warnf("failed to clean up ref")
			return
		}
		log.G(ctx).WithField("key", key).Debugf("cleaned up ref")
	}
	return p, nil
}

type refPool struct {
	path  string
	hosts source.RegistryHosts

	refcounter map[string]*releaser
	cache      *cacheutil.LRUCache
	mu         sync.Mutex
}

type releaser struct {
	count   int
	release func()
}

func (p *refPool) loadRef(ctx context.Context, refspec reference.Spec) (manifest ocispec.Manifest, config ocispec.Image, err error) {
	manifest, config, err = p.readManifestAndConfig(refspec)
	if err == nil {
		log.G(ctx).Debugf("reusing manifest and config of %q", refspec.String())
		return
	}
	log.G(ctx).WithError(err).Debugf("fetching manifest and config of %q", refspec.String())
	manifest, config, err = p.fetchManifestAndConfig(ctx, refspec)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	if err := p.writeManifestAndConfig(refspec, manifest, config); err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	// Cache it so that next immediate call can acquire ref information from that dir.
	p.mu.Lock()
	_, done, _ := p.cache.Add(refspec.String(), refspec)
	p.mu.Unlock()
	go func() {
		// Release it after a reasonable amount of time.
		// If use() funcs are called for this reference, eviction of this won't be done until
		// all corresponding release() funcs are called.
		time.Sleep(defaultManifestCacheTime)
		done()
	}()
	return manifest, config, nil
}

func (p *refPool) use(refspec reference.Spec) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	r, ok := p.refcounter[refspec.String()]
	if !ok {
		_, done, _ := p.cache.Add(refspec.String(), refspec)
		p.refcounter[refspec.String()] = &releaser{
			count:   1,
			release: done,
		}
		return 1
	}
	r.count++
	return r.count
}

func (p *refPool) release(refspec reference.Spec) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	r, ok := p.refcounter[refspec.String()]
	if !ok {
		return 0, fmt.Errorf("ref %q not tracked", refspec.String())
	}
	r.count--
	if r.count <= 0 {
		delete(p.refcounter, refspec.String())
		r.release()
		return 0, nil
	}
	return r.count, nil
}

func (p *refPool) readManifestAndConfig(refspec reference.Spec) (manifest ocispec.Manifest, config ocispec.Image, _ error) {
	mPath, cPath := p.manifestFile(refspec), p.configFile(refspec)
	mf, err := os.Open(mPath)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	defer mf.Close()
	if err := json.NewDecoder(mf).Decode(&manifest); err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	cf, err := os.Open(cPath)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	defer cf.Close()
	if err := json.NewDecoder(cf).Decode(&config); err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	return manifest, config, nil
}

func (p *refPool) writeManifestAndConfig(refspec reference.Spec, manifest ocispec.Manifest, config ocispec.Image) error {
	mPath, cPath := p.manifestFile(refspec), p.configFile(refspec)
	if err := os.MkdirAll(filepath.Dir(mPath), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cPath), 0700); err != nil {
		return err
	}
	mf, err := os.Create(mPath)
	if err != nil {
		return err
	}
	defer mf.Close()
	if err := json.NewEncoder(mf).Encode(&manifest); err != nil {
		return err
	}
	cf, err := os.Create(cPath)
	if err != nil {
		return err
	}
	defer cf.Close()
	return json.NewEncoder(cf).Encode(&config)
}

func (p *refPool) fetchManifestAndConfig(ctx context.Context, refspec reference.Spec) (ocispec.Manifest, ocispec.Image, error) {
	// temporary resolver. should only be used for resolving `refpec`.
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: func(host string) ([]docker.RegistryHost, error) {
			if host != refspec.Hostname() {
				return nil, fmt.Errorf("unexpected host %q for image ref %q", host, refspec.String())
			}
			return p.hosts(refspec)
		},
	})
	_, img, err := resolver.Resolve(ctx, refspec.String())
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	fetcher, err := resolver.Fetcher(ctx, refspec.String())
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	plt := platforms.DefaultSpec() // TODO: should we make this configurable?
	manifest, err := containerdutil.FetchManifestPlatform(ctx, fetcher, img, plt)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	r, err := fetcher.Fetch(ctx, manifest.Config)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}
	defer r.Close()
	var config ocispec.Image
	if err := json.NewDecoder(r).Decode(&config); err != nil {
		return ocispec.Manifest{}, ocispec.Image{}, err
	}

	return manifest, config, nil
}

func (p *refPool) root() string {
	return p.path
}

func (p *refPool) metadataDir(refspec reference.Spec) string {
	return filepath.Join(p.path, "metadata--"+colon2dash(digest.FromString(refspec.String()).String()))
}

func (p *refPool) manifestFile(refspec reference.Spec) string {
	return filepath.Join(p.metadataDir(refspec), "manifest")
}

func (p *refPool) configFile(refspec reference.Spec) string {
	return filepath.Join(p.metadataDir(refspec), "config")
}
