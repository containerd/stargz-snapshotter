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
	"context"
	"encoding/base64"
	"strings"
	"syscall"

	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/stargz-snapshotter/estargz"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	defaultLinkMode = syscall.S_IFLNK | 0400 // -r--------
	defaultDirMode  = syscall.S_IFDIR | 0500 // dr-x------

	poolLink          = "pool"
	chainLink         = "chain"
	layerLink         = "diff"
	debugManifestLink = "manifest"
	debugConfigLink   = "config"
	layerInfoLink     = "info"
	layerUseFile      = "use"
)

// node is a filesystem inode abstraction.
type rootnode struct {
	fusefs.Inode
	pool *pool
}

var _ = (fusefs.InodeEmbedder)((*rootnode)(nil))

var _ = (fusefs.NodeLookuper)((*rootnode)(nil))

// Lookup loads manifest and config of specified name (imgae reference)
// and returns refnode of the specified name
func (n *rootnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	switch name {
	case poolLink:
		return n.NewInode(ctx,
			&linknode{linkname: n.pool.root()}, defaultLinkAttr(&out.Attr)), 0
	}
	refBytes, err := base64.StdEncoding.DecodeString(name)
	if err != nil {
		log.G(ctx).WithError(err).Debugf("failed to decode ref base64 %q", name)
		return nil, syscall.EINVAL
	}
	ref := string(refBytes)
	refspec, err := reference.Parse(ref)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("invalid reference %q for %q", ref, name)
		return nil, syscall.EINVAL
	}
	manifest, mPath, config, cPath, err := n.pool.loadManifestAndConfig(ctx, refspec)
	if err != nil {
		log.G(ctx).WithError(err).
			Warnf("failed to fetch manifest and config of %q(%q)", ref, name)
		return nil, syscall.EIO
	}
	return n.NewInode(ctx, &refnode{
		pool:         n.pool,
		ref:          refspec,
		manifest:     manifest,
		manifestPath: mPath,
		config:       config,
		configPath:   cPath,
	}, defaultDirAttr(&out.Attr)), 0
}

// node is a filesystem inode abstraction.
type refnode struct {
	fusefs.Inode
	pool *pool

	ref          reference.Spec
	manifest     ocispec.Manifest
	manifestPath string
	config       ocispec.Image
	configPath   string
}

var _ = (fusefs.InodeEmbedder)((*refnode)(nil))

var _ = (fusefs.NodeLookuper)((*refnode)(nil))

// Lookup returns layernode of the specified name
func (n *refnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	switch name {
	case debugManifestLink:
		return n.NewInode(ctx,
			&linknode{linkname: n.manifestPath}, defaultLinkAttr(&out.Attr)), 0
	case debugConfigLink:
		return n.NewInode(ctx,
			&linknode{linkname: n.configPath}, defaultLinkAttr(&out.Attr)), 0
	}
	targetDigest, err := digest.Parse(name)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("invalid digest for %q", name)
		return nil, syscall.EINVAL
	}
	var chain []ocispec.Descriptor
	var found bool
	for _, l := range n.manifest.Layers {
		chain = append(chain, l)
		if l.Digest == targetDigest {
			found = true
			break
		}
	}
	if !found {
		log.G(ctx).WithError(err).Warnf("invalid digest for %q: %q", name, targetDigest.String())
		return nil, syscall.EINVAL
	}
	return n.NewInode(ctx, &layernode{
		pool:    n.pool,
		layer:   chain[len(chain)-1],
		chain:   chain,
		layers:  n.manifest.Layers,
		refnode: n,
	}, defaultDirAttr(&out.Attr)), 0
}

var _ = (fusefs.NodeRmdirer)((*refnode)(nil))

// Rmdir marks this layer as "release".
// We don't use layernode.Unlink because Unlink event doesn't reach here when "use" file isn't visible
// to the filesystem client.
func (n *refnode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if name == debugManifestLink || name == debugConfigLink {
		return syscall.EROFS // nop
	}
	targetDigest, err := digest.Parse(name)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("invalid digest for %q during release", name)
		return syscall.EINVAL
	}
	current, err := n.pool.release(n.ref, targetDigest)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to release layer %v / %v", n.ref, targetDigest)
		return syscall.EIO

	}
	log.G(ctx).WithField("refcounter", current).Warnf("layer %v / %v is marked as RELEASE", n.ref, targetDigest)
	return syscall.ENOENT
}

// node is a filesystem inode abstraction.
type layernode struct {
	fusefs.Inode
	pool *pool

	layer  ocispec.Descriptor
	chain  []ocispec.Descriptor
	layers []ocispec.Descriptor

	refnode *refnode
}

var _ = (fusefs.InodeEmbedder)((*layernode)(nil))

var _ = (fusefs.NodeCreater)((*layernode)(nil))

// Create marks this layer as "using".
// We don't use refnode.Mkdir because Mkdir event doesn't reach here if layernode already exists.
func (n *layernode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fusefs.Inode, fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if name == layerUseFile {
		current := n.pool.use(n.refnode.ref, n.layer.Digest)
		log.G(ctx).WithField("refcounter", current).Warnf("layer %v / %v is marked as USING", n.refnode.ref, n.layer.Digest)
	}

	// TODO: implement cleanup
	return nil, nil, 0, syscall.ENOENT
}

var _ = (fusefs.NodeLookuper)((*layernode)(nil))

// Lookup routes to the target file stored in the pool, based on the specified file name.
func (n *layernode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {

	var linkpath string
	switch name {
	case layerInfoLink:
		var err error
		linkpath, err = n.pool.loadLayerInfo(ctx, n.refnode.ref, n.layer.Digest)
		if err != nil {
			log.G(ctx).WithError(err).
				Warnf("failed to get layer info for %q: %q", name, n.layer.Digest)
			return nil, syscall.EIO
		}
	case layerLink:
		var layers []string
		for _, l := range n.layers {
			if images.IsLayerType(l.MediaType) {
				layers = append(layers, l.Digest.String())
			}
		}
		labels := map[string]string{
			targetImageLayersLabel: strings.Join(layers, ","),
		}
		if n.layer.Annotations != nil {
			tocDigest, ok := n.layer.Annotations[estargz.TOCJSONDigestAnnotation]
			if ok {
				labels[estargz.TOCJSONDigestAnnotation] = tocDigest
			}
		}
		var err error
		linkpath, err = n.pool.loadLayer(ctx, n.refnode.ref, n.layer.Digest, labels)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("failed to mount layer of %q: %q", name, n.layer.Digest)
			return nil, syscall.EIO
		}
	case chainLink:
		var err error
		linkpath, err = n.pool.loadChain(ctx, n.refnode.ref, n.chain)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("failed to mount chain of %q: %q",
				name, n.layer.Digest)
			return nil, syscall.EIO
		}
	case layerUseFile:
		log.G(ctx).Debugf("\"use\" file is referred but return ENOENT for reference management")
		return nil, syscall.ENOENT
	default:
		log.G(ctx).Warnf("unknown filename %q", name)
		return nil, syscall.ENOENT
	}
	return n.NewInode(ctx, &linknode{linkname: linkpath}, defaultLinkAttr(&out.Attr)), 0
}

type linknode struct {
	fusefs.Inode
	linkname string
}

var _ = (fusefs.InodeEmbedder)((*linknode)(nil))

var _ = (fusefs.NodeReadlinker)((*linknode)(nil))

func (n *linknode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return []byte(n.linkname), 0 // TODO: linkname shouldn't statically embedded?
}

func defaultDirAttr(out *fuse.Attr) fusefs.StableAttr {
	// out.Ino
	out.Size = 0
	// out.Blksize
	// out.Blocks
	// out.Nlink
	out.Mode = defaultDirMode
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	// out.Mtime
	// out.Mtimensec
	// out.Rdev
	// out.Padding

	return fusefs.StableAttr{
		Mode: out.Mode,
	}
}

func defaultLinkAttr(out *fuse.Attr) fusefs.StableAttr {
	// out.Ino
	out.Size = 0
	// out.Blksize
	// out.Blocks
	// out.Nlink
	out.Mode = defaultLinkMode
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	// out.Mtime
	// out.Mtimensec
	// out.Rdev
	// out.Padding

	return fusefs.StableAttr{
		Mode: out.Mode,
	}
}
