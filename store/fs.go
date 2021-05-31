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
	"encoding/base64"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/fs/layer"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	defaultLinkMode = syscall.S_IFLNK | 0400 // -r--------
	defaultDirMode  = syscall.S_IFDIR | 0500 // dr-x------
	layerFileMode   = 0400                   // -r--------
	blockSize       = 4096

	poolLink          = "pool"
	layerLink         = "diff"
	blobLink          = "blob"
	debugManifestLink = "manifest"
	debugConfigLink   = "config"
	layerInfoLink     = "info"
	layerUseFile      = "use"

	fusermountBin = "fusermount"
)

func Mount(ctx context.Context, mountpoint string, pool *Pool, debug bool) error {
	timeSec := time.Second
	rawFS := fusefs.NewNodeFS(&rootnode{pool: pool}, &fusefs.Options{
		AttrTimeout:     &timeSec,
		EntryTimeout:    &timeSec,
		NullPermissions: true,
	})
	mountOpts := &fuse.MountOptions{
		AllowOther: true, // allow users other than root&mounter to access fs
		FsName:     "stargzstore",
		Debug:      debug,
	}
	if _, err := exec.LookPath(fusermountBin); err == nil {
		mountOpts.Options = []string{"suid"} // option for fusermount; allow setuid inside container
	} else {
		log.G(ctx).WithError(err).Debugf("%s not installed; trying direct mount", fusermountBin)
		mountOpts.DirectMount = true
	}
	server, err := fuse.NewServer(rawFS, mountpoint, mountOpts)
	if err != nil {
		return err
	}
	go server.Serve()
	return server.WaitMount()
}

// rootnode is the mountpoint node of stargz-store.w
type rootnode struct {
	fusefs.Inode
	pool *Pool
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

// refnode is the node at <mountpoint>/<imageref>.
type refnode struct {
	fusefs.Inode
	pool *Pool

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
	var layer *ocispec.Descriptor
	for _, l := range n.manifest.Layers {
		if l.Digest == targetDigest {
			layer = &l
			break
		}
	}
	if layer == nil {
		log.G(ctx).WithError(err).Warnf("invalid digest for %q: %q", name, targetDigest.String())
		return nil, syscall.EINVAL
	}
	return n.NewInode(ctx, &layernode{
		pool:    n.pool,
		layer:   *layer,
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

// layernode is the node at <mountpoint>/<imageref>/<layerdigest>.
type layernode struct {
	fusefs.Inode
	pool *Pool

	layer  ocispec.Descriptor
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
		log.G(ctx).WithField("refcounter", current).Warnf("layer %v / %v is marked as USING",
			n.refnode.ref, n.layer.Digest)
	}

	// TODO: implement cleanup
	return nil, nil, 0, syscall.ENOENT
}

var _ = (fusefs.NodeLookuper)((*layernode)(nil))

// Lookup routes to the target file stored in the pool, based on the specified file name.
func (n *layernode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	switch name {
	case layerInfoLink:
		var err error
		infopath, err := n.pool.loadLayerInfo(ctx, n.refnode.ref, n.layer.Digest)
		if err != nil {
			log.G(ctx).WithError(err).
				Warnf("failed to get layer info for %q: %q", name, n.layer.Digest)
			return nil, syscall.EIO
		}
		return n.NewInode(ctx, &linknode{linkname: infopath}, defaultLinkAttr(&out.Attr)), 0
	case layerLink, blobLink:
		l, err := n.pool.loadLayer(ctx, n.refnode.ref, n.layer, n.layers)
		if err != nil {
			cErr := ctx.Err()
			if errors.Is(cErr, context.Canceled) || errors.Is(err, context.Canceled) {
				// When filesystem client canceled to lookup this layer,
				// do not log this as "preparation failure" because it's
				// intensional.
				log.G(ctx).WithError(err).
					Debugf("error resolving layer (context error: %v)", cErr)
				return nil, syscall.EIO
			}
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithField("layerdigest", n.layer.Digest).
				WithError(err).
				Debugf("error resolving layer (context error: %v)", cErr)
			log.G(ctx).WithError(err).Warnf("failed to mount layer %q: %q",
				name, n.layer.Digest)
			return nil, syscall.EIO
		}
		if name == blobLink {
			return n.NewInode(ctx, &blobnode{l: l}, layerToAttr(l, &out.Attr)), 0
		}
		root, err := l.RootNode()
		if err != nil {
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithField("layerdigest", n.layer.Digest).
				WithError(err).
				Debugf("failed to get root node")
			return nil, syscall.EIO
		}
		var ao fuse.AttrOut
		if errno := root.(fusefs.NodeGetattrer).Getattr(ctx, nil, &ao); errno != 0 {
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithField("layerdigest", n.layer.Digest).
				WithError(err).
				Debugf("failed to get root node")
			return nil, errno
		}
		copyAttr(&out.Attr, &ao.Attr)
		return n.NewInode(ctx, root, fusefs.StableAttr{
			Mode: out.Attr.Mode,
			Ino:  out.Attr.Ino,
		}), 0
	case layerUseFile:
		log.G(ctx).Debugf("\"use\" file is referred but return ENOENT for reference management")
		return nil, syscall.ENOENT
	default:
		log.G(ctx).Warnf("unknown filename %q", name)
		return nil, syscall.ENOENT
	}
}

// blobnode is a regular file node that contains raw blob data
type blobnode struct {
	fusefs.Inode
	l layer.Layer
}

var _ = (fusefs.InodeEmbedder)((*blobnode)(nil))

var _ = (fusefs.NodeOpener)((*blobnode)(nil))

func (n *blobnode) Open(ctx context.Context, flags uint32) (fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	return &blobfile{l: n.l}, 0, 0
}

// blob file is the file handle of blob contents.
type blobfile struct {
	l layer.Layer
}

var _ = (fusefs.FileReader)((*blobfile)(nil))

func (f *blobfile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	s, err := f.l.ReadAt(dest, off,
		remote.WithContext(ctx),              // Make cancellable
		remote.WithCacheOpts(cache.Direct()), // Do not pollute mem cache
	)
	if err != nil && err != io.EOF {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:s]), 0
}

var _ = (fusefs.FileGetattrer)((*blobfile)(nil))

func (f *blobfile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	layerToAttr(f.l, &out.Attr)
	return 0
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

func copyAttr(dest, src *fuse.Attr) {
	dest.Ino = src.Ino
	dest.Size = src.Size
	dest.Blocks = src.Blocks
	dest.Atime = src.Atime
	dest.Mtime = src.Mtime
	dest.Ctime = src.Ctime
	dest.Atimensec = src.Atimensec
	dest.Mtimensec = src.Mtimensec
	dest.Ctimensec = src.Ctimensec
	dest.Mode = src.Mode
	dest.Nlink = src.Nlink
	dest.Owner = src.Owner
	dest.Rdev = src.Rdev
	dest.Blksize = src.Blksize
	dest.Padding = src.Padding
}

func layerToAttr(l layer.Layer, out *fuse.Attr) fusefs.StableAttr {
	// out.Ino
	out.Size = uint64(l.Info().Size)
	out.Blksize = blockSize
	out.Blocks = out.Size / uint64(out.Blksize)
	if out.Size%uint64(out.Blksize) > 0 {
		out.Blocks++
	}
	out.Nlink = 1
	out.Mode = layerFileMode
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	// out.Mtime
	// out.Mtimensec
	// out.Rdev
	// out.Padding

	return fusefs.StableAttr{
		Mode: out.Mode,
	}
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
