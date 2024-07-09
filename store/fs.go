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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/reference"
	"github.com/containerd/log"
	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/fs/layer"
	"github.com/containerd/stargz-snapshotter/fs/remote"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sync/singleflight"
)

const (
	defaultLinkMode = syscall.S_IFLNK | 0400 // -r--------
	defaultDirMode  = syscall.S_IFDIR | 0500 // dr-x------
	defaultFileMode = 0400                   // -r--------
	layerFileMode   = 0400                   // -r--------
	blockSize       = 4096

	poolLink      = "pool"
	layerLink     = "diff"
	blobLink      = "blob"
	layerInfoLink = "info"
	layerUseFile  = "use"

	fusermountBin = "fusermount"
)

func Mount(ctx context.Context, mountpoint string, layerManager *LayerManager, debug bool) error {
	timeSec := time.Second
	rawFS := fusefs.NewNodeFS(&rootnode{
		fs: &fs{
			layerManager: layerManager,
			nodeMap:      new(idMap),
			layerMap:     new(idMap),
		},
	}, &fusefs.Options{
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

type fs struct {
	layerManager *LayerManager

	// nodeMap manages inode numbers for nodes other than nodes in layers
	// (i.e. nodes other than ones inside `diff` directories).
	// - inode number = [ 0 ][ uint32 ID ]
	nodeMap *idMap
	// layerMap manages upper bits of inode numbers for nodes inside layers.
	// - inode number = [ uint32 layer ID ][ uint32 number (unique inside `diff` directory) ]
	// inodes numbers of noeds inside each `diff` directory are prefixed by an unique uint32
	// so that they don't conflict with nodes outside `diff` directories.
	layerMap *idMap

	knownNode   map[string]map[string]*layerReleasable
	knownNodeMu sync.Mutex
}

type layerReleasable struct {
	n        fusefs.InodeEmbedder
	released bool
	mu       sync.Mutex
}

func (lh *layerReleasable) releasable() bool {
	lh.mu.Lock()
	released := lh.released
	lh.mu.Unlock()
	return released && isForgotten(lh.n.EmbeddedInode())
}

func (lh *layerReleasable) release() {
	lh.mu.Lock()
	lh.released = true
	lh.mu.Unlock()
}

func isForgotten(n *fusefs.Inode) bool {
	if !n.Forgotten() {
		return false
	}
	for _, cn := range n.Children() {
		if !isForgotten(cn) {
			return false
		}
	}
	return true
}

type inoReleasable struct {
	n fusefs.InodeEmbedder
}

func (r *inoReleasable) releasable() bool {
	return r.n.EmbeddedInode().Forgotten()
}

func (fs *fs) newInodeWithID(ctx context.Context, p func(uint32) fusefs.InodeEmbedder) (*fusefs.Inode, syscall.Errno) {
	var ino fusefs.InodeEmbedder
	if err := fs.nodeMap.add(func(id uint32) (releasable, error) {
		ino = p(id)
		return &inoReleasable{ino}, nil
	}); err != nil || ino == nil {
		log.G(ctx).WithError(err).Debug("cannot generate ID")
		return nil, syscall.EIO
	}
	return ino.EmbeddedInode(), 0
}

// rootnode is the mountpoint node of stargz-store.
type rootnode struct {
	fusefs.Inode
	fs *fs
}

var _ = (fusefs.InodeEmbedder)((*rootnode)(nil))

var _ = (fusefs.NodeLookuper)((*rootnode)(nil))

// Lookup loads manifest and config of specified name (image reference)
// and returns refnode of the specified name
func (n *rootnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	// lookup on memory nodes
	if cn := n.GetChild(name); cn != nil {
		switch tn := cn.Operations().(type) {
		case *fusefs.MemSymlink:
			copyAttr(&out.Attr, &tn.Attr)
		case *refnode:
			copyAttr(&out.Attr, &tn.attr)
		default:
			log.G(ctx).Warn("rootnode.Lookup: uknown node type detected")
			return nil, syscall.EIO
		}
		out.Attr.Ino = cn.StableAttr().Ino
		return cn, 0
	}
	switch name {
	case poolLink:
		sAttr := defaultLinkAttr(&out.Attr)
		cn := &fusefs.MemSymlink{Data: []byte(n.fs.layerManager.refPool.root())}
		copyAttr(&cn.Attr, &out.Attr)
		return n.fs.newInodeWithID(ctx, func(ino uint32) fusefs.InodeEmbedder {
			out.Attr.Ino = uint64(ino)
			cn.Attr.Ino = uint64(ino)
			sAttr.Ino = uint64(ino)
			return n.NewInode(ctx, cn, sAttr)
		})
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
	sAttr := defaultDirAttr(&out.Attr)
	cn := &refnode{
		fs:  n.fs,
		ref: refspec,
	}
	copyAttr(&cn.attr, &out.Attr)
	return n.fs.newInodeWithID(ctx, func(ino uint32) fusefs.InodeEmbedder {
		out.Attr.Ino = uint64(ino)
		cn.attr.Ino = uint64(ino)
		sAttr.Ino = uint64(ino)
		return n.NewInode(ctx, cn, sAttr)
	})
}

// refnode is the node at <mountpoint>/<imageref>.
type refnode struct {
	fusefs.Inode
	fs   *fs
	attr fuse.Attr

	ref reference.Spec
}

var _ = (fusefs.InodeEmbedder)((*refnode)(nil))

var _ = (fusefs.NodeLookuper)((*refnode)(nil))

// Lookup returns layernode of the specified name
func (n *refnode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	// lookup on memory nodes
	if cn := n.GetChild(name); cn != nil {
		switch tn := cn.Operations().(type) {
		case *layernode:
			copyAttr(&out.Attr, &tn.attr)
		default:
			log.G(ctx).Warn("refnode.Lookup: uknown node type detected")
			return nil, syscall.EIO
		}
		out.Attr.Ino = cn.StableAttr().Ino
		return cn, 0
	}
	targetDigest, err := digest.Parse(name)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("invalid digest for %q", name)
		return nil, syscall.EINVAL
	}
	sAttr := defaultDirAttr(&out.Attr)
	cn := &layernode{
		fs:      n.fs,
		digest:  targetDigest,
		refnode: n,
	}
	copyAttr(&cn.attr, &out.Attr)
	return n.fs.newInodeWithID(ctx, func(ino uint32) fusefs.InodeEmbedder {
		out.Attr.Ino = uint64(ino)
		cn.attr.Ino = uint64(ino)
		sAttr.Ino = uint64(ino)
		return n.NewInode(ctx, cn, sAttr)
	})
}

var _ = (fusefs.NodeRmdirer)((*refnode)(nil))

// Rmdir marks this layer as "release".
// We don't use layernode.Unlink because Unlink event doesn't reach here when "use" file isn't visible
// to the filesystem client.
func (n *refnode) Rmdir(ctx context.Context, name string) syscall.Errno {
	targetDigest, err := digest.Parse(name)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("invalid digest for %q during release", name)
		return syscall.EINVAL
	}
	current, err := n.fs.layerManager.release(ctx, n.ref, targetDigest)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to release layer %v / %v", n.ref, targetDigest)
		return syscall.EIO
	}
	if current == 0 {
		n.fs.knownNodeMu.Lock()
		lh, ok := n.fs.knownNode[n.ref.String()][targetDigest.String()]
		if !ok {
			n.fs.knownNodeMu.Unlock()
			log.G(ctx).WithError(err).Warnf("node of layer %v/%v is not registered", n.ref, targetDigest)
			return syscall.EIO
		}
		lh.release()
		delete(n.fs.knownNode[n.ref.String()], targetDigest.String())
		if len(n.fs.knownNode[n.ref.String()]) == 0 {
			delete(n.fs.knownNode, n.ref.String())
		}
		n.fs.knownNodeMu.Unlock()
	}
	log.G(ctx).WithField("refcounter", current).Infof("layer %v/%v is marked as RELEASE", n.ref, targetDigest)
	return syscall.ENOENT
}

// layernode is the node at <mountpoint>/<imageref>/<layerdigest>.
type layernode struct {
	fusefs.Inode
	attr fuse.Attr
	fs   *fs

	refnode *refnode
	digest  digest.Digest
}

var _ = (fusefs.InodeEmbedder)((*layernode)(nil))

var _ = (fusefs.NodeCreater)((*layernode)(nil))

// Create marks this layer as "using".
// We don't use refnode.Mkdir because Mkdir event doesn't reach here if layernode already exists.
func (n *layernode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fusefs.Inode, fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if name == layerUseFile {
		current := n.fs.layerManager.use(n.refnode.ref, n.digest)
		log.G(ctx).WithField("refcounter", current).Infof("layer %v / %v is marked as USING", n.refnode.ref, n.digest)
	}
	return nil, nil, 0, syscall.ENOENT
}

var _ = (fusefs.NodeLookuper)((*layernode)(nil))

// Lookup routes to the target file stored in the pool, based on the specified file name.
func (n *layernode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	switch name {
	case layerInfoLink:
		info, err := n.fs.layerManager.getLayerInfo(ctx, n.refnode.ref, n.digest)
		if err != nil {
			log.G(ctx).WithError(err).Warnf("failed to get layer info for %q: %q", name, n.digest)
			return nil, syscall.EIO
		}
		buf := new(bytes.Buffer)
		if err := json.NewEncoder(buf).Encode(&info); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to encode layer info for %q: %q", name, n.digest)
			return nil, syscall.EIO
		}
		infoData := buf.Bytes()
		sAttr := defaultFileAttr(uint64(len(infoData)), &out.Attr)
		cn := &fusefs.MemRegularFile{Data: infoData}
		copyAttr(&cn.Attr, &out.Attr)
		return n.fs.newInodeWithID(ctx, func(ino uint32) fusefs.InodeEmbedder {
			out.Attr.Ino = uint64(ino)
			cn.Attr.Ino = uint64(ino)
			sAttr.Ino = uint64(ino)
			return n.NewInode(ctx, cn, sAttr)
		})
	case layerLink, blobLink:

		// Check if layer is already known
		if name == layerLink {
			n.fs.knownNodeMu.Lock()
			if lh, ok := n.fs.knownNode[n.refnode.ref.String()][n.digest.String()]; ok {
				var ao fuse.AttrOut
				if errno := lh.n.(fusefs.NodeGetattrer).Getattr(ctx, nil, &ao); errno != 0 {
					return nil, errno
				}
				copyAttr(&out.Attr, &ao.Attr)
				n.fs.knownNodeMu.Unlock()
				return n.NewInode(ctx, lh.n, fusefs.StableAttr{
					Mode: out.Attr.Mode,
					Ino:  out.Attr.Ino,
				}), 0
			}
			n.fs.knownNodeMu.Unlock()
		}

		// Resolve layer
		l, err := n.fs.layerManager.getLayer(ctx, n.refnode.ref, n.digest)
		if err != nil {
			cErr := ctx.Err()
			if errors.Is(cErr, context.Canceled) || errors.Is(err, context.Canceled) {
				// When filesystem client canceled to lookup this layer,
				// do not log this as "preparation failure" because it's
				// intensional.
				log.G(ctx).WithError(err).Debugf("error resolving layer (context error: %v)", cErr)
				return nil, syscall.EIO
			}
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithField("digest", n.digest).
				WithError(err).
				Debugf("error resolving layer (context error: %v)", cErr)
			log.G(ctx).WithError(err).Warnf("failed to mount layer %q: %q", name, n.digest)
			return nil, syscall.EIO
		}
		if err := l.Verify(n.digest); err != nil {
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithField("digest", n.digest).
				WithError(err).
				Debugf("failed to verify layer")
			log.G(ctx).WithError(err).Warnf("failed to mount layer %q: %q", name, n.digest)
			return nil, syscall.EIO
		}
		if name == blobLink {
			sAttr := layerToAttr(l, &out.Attr)
			cn := &blobnode{l: l}
			copyAttr(&cn.attr, &out.Attr)
			return n.fs.newInodeWithID(ctx, func(ino uint32) fusefs.InodeEmbedder {
				out.Attr.Ino = uint64(ino)
				cn.attr.Ino = uint64(ino)
				sAttr.Ino = uint64(ino)
				return n.NewInode(ctx, cn, sAttr)
			})
		}

		var cn *fusefs.Inode
		var errno syscall.Errno
		err = n.fs.layerMap.add(func(id uint32) (releasable, error) {
			root, err := l.RootNode(id)
			if err != nil {
				return nil, err
			}

			var ao fuse.AttrOut
			errno = root.(fusefs.NodeGetattrer).Getattr(ctx, nil, &ao)
			if errno != 0 {
				return nil, fmt.Errorf("failed to get root node: %v", errno)
			}

			copyAttr(&out.Attr, &ao.Attr)
			cn = n.NewInode(ctx, root, fusefs.StableAttr{
				Mode: out.Attr.Mode,
				Ino:  out.Attr.Ino,
			})

			rr := &layerReleasable{n: root}
			n.fs.knownNodeMu.Lock()
			if n.fs.knownNode == nil {
				n.fs.knownNode = make(map[string]map[string]*layerReleasable)
			}
			if n.fs.knownNode[n.refnode.ref.String()] == nil {
				n.fs.knownNode[n.refnode.ref.String()] = make(map[string]*layerReleasable)
			}
			n.fs.knownNode[n.refnode.ref.String()][n.digest.String()] = rr
			n.fs.knownNodeMu.Unlock()
			return rr, nil
		})
		if err != nil || errno != 0 {
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareFailed).
				WithField("layerdigest", n.digest).
				WithError(err).
				WithField("errno", errno).
				Debugf("failed to get root node")
			if errno == 0 {
				errno = syscall.EIO
			}
			return nil, errno
		}
		return cn, 0
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
	l    layer.Layer
	attr fuse.Attr
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

func defaultFileAttr(size uint64, out *fuse.Attr) fusefs.StableAttr {
	// out.Ino
	out.Size = size
	out.Blksize = blockSize
	out.Blocks = out.Size / uint64(out.Blksize)
	if out.Size%uint64(out.Blksize) > 0 {
		out.Blocks++
	}
	out.Nlink = 1
	out.Mode = defaultFileMode
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

// idMap manages uint32 IDs with automatic GC for releasable objects.
type idMap struct {
	m        map[uint32]releasable
	max      uint32
	mu       sync.Mutex
	cleanupG singleflight.Group
}

type releasable interface {
	releasable() bool
}

// add reserves an unique uint32 object for the provided releasable object.
// when that object become releasable, that ID will be reused for other objects.
func (m *idMap) add(p func(uint32) (releasable, error)) error {
	m.cleanupG.Do("cleanup", func() (interface{}, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		max := uint32(0)
		for i := uint32(0); i <= m.max; i++ {
			if e, ok := m.m[i]; ok {
				if e.releasable() {
					delete(m.m, i)
				} else {
					max = i
				}
			}
		}
		m.max = max
		return nil, nil
	})

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.m == nil {
		m.m = make(map[uint32]releasable)
	}

	for i := uint32(0); i <= ^uint32(0); i++ {
		if i == 0 {
			continue
		}
		e, ok := m.m[i]
		if !ok || e.releasable() {
			r, err := p(i)
			if err != nil {
				return err
			}
			if m.max < i {
				m.max = i
			}
			m.m[i] = r
			return nil
		}
	}
	return fmt.Errorf("no ID is usable")
}
