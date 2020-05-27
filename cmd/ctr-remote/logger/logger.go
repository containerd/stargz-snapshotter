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

package logger

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/util"
	fusefs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const (
	DefaultBlockSize      = 4096
	whiteoutPrefix        = ".wh."
	whiteoutOpaqueDir     = whiteoutPrefix + whiteoutPrefix + ".opq"
	xattrPAXRecordsPrefix = "SCHILY.xattr."
)

func Mount(mountPoint string, tarfile io.ReaderAt, monitor Monitor) (func() error, error) {
	// Mount filesystem.
	root := newRoot(tarfile, monitor)
	timeSec := time.Second
	rawFS := fusefs.NewNodeFS(root, &fusefs.Options{
		AttrTimeout:     &timeSec,
		EntryTimeout:    &timeSec,
		NullPermissions: true,
	})
	if err := root.InitNodes(); err != nil {
		return nil, errors.Wrap(err, "failed to init nodes")
	}
	server, err := fuse.NewServer(rawFS, mountPoint, &fuse.MountOptions{
		AllowOther: true,             // allow users other than root&mounter to access fs
		Options:    []string{"suid"}, // allow setuid inside container
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to prepare filesystem server")
	}
	go server.Serve()
	if err := server.WaitMount(); err != nil {
		return nil, errors.Wrap(err, "failed to mount filesystem")
	}

	return func() error { return server.Unmount() }, nil
}

// Nodes
func newRoot(tarfile io.ReaderAt, monitor Monitor) *node {
	root := &node{
		tr:      tarfile,
		monitor: monitor,
	}
	now := time.Now()
	root.attr.SetTimes(&now, &now, &now)
	root.attr.Mode = fuse.S_IFDIR | 0777

	return root
}

type node struct {
	fusefs.Inode

	fullname string
	attr     fuse.Attr
	r        io.ReaderAt
	link     string // Symbolic link.
	xattr    map[string][]byte

	tr io.ReaderAt

	monitor Monitor
}

var _ = (fusefs.InodeEmbedder)((*node)(nil))

var _ = (fusefs.NodeLookuper)((*node)(nil))

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fusefs.Inode, syscall.Errno) {
	c := n.GetChild(name)
	if c == nil {
		return nil, syscall.ENOENT
	}
	// All node used in this filesystem must have Getattr method.
	var a fuse.AttrOut
	if errno := c.Operations().(fusefs.NodeGetattrer).Getattr(ctx, nil, &a); errno != 0 {
		return nil, errno
	}
	out.Attr = a.Attr
	n.monitor.OnLookup(filepath.Join(n.fullname, name))

	return c, 0
}

var _ = (fusefs.NodeReadlinker)((*node)(nil))

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	n.monitor.OnReadlink(n.fullname, n.link)
	return []byte(n.link), 0
}

var _ = (fusefs.NodeOpener)((*node)(nil))

func (n *node) Open(ctx context.Context, flags uint32) (fh fusefs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	n.monitor.OnOpen(n.fullname)
	// It is OK to return (nil, OK) here. In that case,
	// this node need to implement "Read" function.
	return nil, 0, 0
}

var _ = (fusefs.NodeReader)((*node)(nil))

func (n *node) Read(ctx context.Context, f fusefs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if _, err := n.r.ReadAt(dest, off); err != nil && err != io.EOF {
		fmt.Printf("Error: Failed to read %s: %v", n.fullname, err)
		return nil, syscall.EIO
	}
	n.monitor.OnRead(n.fullname, off, int64(len(dest)))
	return fuse.ReadResultData(dest), 0
}

var _ = (fusefs.NodeGetattrer)((*node)(nil))

func (n *node) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	copyAttr(&out.Attr, n.attr)
	n.monitor.OnGetAttr(n.fullname)
	return 0
}

var _ = (fusefs.NodeGetxattrer)((*node)(nil))

func (n *node) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	if v, ok := n.xattr[attr]; ok {
		if len(dest) < len(v) {
			return uint32(len(v)), syscall.ERANGE
		}
		return uint32(copy(dest, v)), 0
	}
	return 0, syscall.ENODATA
}

var _ = (fusefs.NodeListxattrer)((*node)(nil))

func (n *node) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	var attrs []byte
	for k := range n.xattr {
		attrs = append(attrs, []byte(k+"\x00")...)
	}
	if len(dest) < len(attrs) {
		return uint32(len(attrs)), syscall.ERANGE
	}
	return uint32(copy(dest, attrs)), 0
}

var _ = (fusefs.NodeStatfser)((*node)(nil))

func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {

	// http://man7.org/linux/man-pages/man2/statfs.2.html
	out.Blocks = 0 // dummy
	out.Bfree = 0
	out.Bavail = 0
	out.Files = 0 // dummy
	out.Ffree = 0
	out.Bsize = 0 // dummy
	out.NameLen = 1<<32 - 1
	out.Frsize = 0 // dummy
	out.Padding = 0
	out.Spare = [6]uint32{}

	return 0
}

func (n *node) InitNodes() error {
	ctx := context.Background()

	pw, err := util.NewPositionWatcher(n.tr)
	if err != nil {
		return errors.Wrap(err, "Failed to make position watcher")
	}
	tr := tar.NewReader(pw)

	// Walk functions for nodes
	getormake := func(n *fusefs.Inode, base string) (c *fusefs.Inode, err error) {
		if c = n.GetChild(base); c == nil {
			// Make temporary dummy node (directory).
			if ok := n.AddChild(base, n.NewPersistentInode(ctx, &node{}, fusefs.StableAttr{
				Mode: syscall.S_IFDIR,
			}), true); !ok {
				return nil, fmt.Errorf("failed to add dummy child %q", base)
			}
			if c = n.GetChild(base); c == nil {
				return nil, fmt.Errorf("dummy child %q hasn't been registered", base)
			}
		}
		return
	}
	getnode := func(n *fusefs.Inode, base string) (c *fusefs.Inode, err error) {
		if c = n.GetChild(base); c == nil {
			// the original node isn't found. We do not allow it.
			return nil, fmt.Errorf("Node %q not found", base)
		}
		return
	}

	var whiteouts []string

	// Walk through all nodes.
	for {
		// Fetch and parse next header.
		h, err := tr.Next()
		if err != nil {
			if err != io.EOF {
				return errors.Wrap(err, "failed to parse tar file")
			}
			break
		}
		var (
			fullname     = filepath.Clean(h.Name)
			dir, base    = filepath.Split(fullname)
			parentDir    *fusefs.Inode
			existingNode *fusefs.Inode
			xattrs       = make(map[string][]byte)
		)
		if parentDir, err = n.walkDown(dir, getormake); err != nil {
			return errors.Wrap(err, "failed to make dummy nodes")
		}
		existingNode = parentDir.GetChild(base)
		if h.PAXRecords != nil {
			for k, v := range h.PAXRecords {
				if strings.HasPrefix(k, xattrPAXRecordsPrefix) {
					xattrs[k[len(xattrPAXRecordsPrefix):]] = []byte(v)
				}
			}
		}
		switch {
		case existingNode != nil:
			if !existingNode.IsDir() {
				return fmt.Errorf("node %q is placeholder but not a directory", fullname)
			}
			// This is "placeholder node" which has been registered in previous loop as
			// an intermediate directory. Now we update it with real one.
			ph, ok := existingNode.Operations().(*node)
			if !ok {
				return fmt.Errorf("invalid placeholder node type for %q", fullname)
			}
			ph.fullname = fullname
			ph.attr, _ = headerToAttr(h)
			ph.link = h.Linkname
			ph.xattr = override(ph.xattr, xattrs) // preserve previously assigned xattr
			ph.tr = n.tr
			ph.monitor = n.monitor
		case strings.HasPrefix(base, whiteoutPrefix):
			if base == whiteoutOpaqueDir {
				// This node is opaque directory indicator so we append opaque xattr to
				// the parent directory.
				pn, ok := parentDir.Operations().(*node)
				if !ok {
					return fmt.Errorf("parent node %q isn't valid node", dir)
				}
				if pn.xattr == nil {
					pn.xattr = make(map[string][]byte)
				}
				pn.xattr["trusted.overlay.opaque"] = []byte("y")
			} else {
				// This node is a whiteout, so we don't want to show it.
				// We record it now and then hide the target node later.
				whiteouts = append(whiteouts, fullname)
			}
		case h.Typeflag == tar.TypeLink:
			if h.Linkname == "" {
				return fmt.Errorf("Linkname of hardlink %q is not found", fullname)
			}
			// This node is a hardlink. Same as major tar tools(GNU tar etc.),
			// we pretend that the target node of this hard link has already been appeared.
			target, err := n.walkDown(filepath.Clean(h.Linkname), getnode)
			if err != nil {
				return errors.Wrapf(err, "hardlink(%q ==> %q) is not found",
					fullname, h.Linkname)
			}
			n, ok := target.Operations().(*node)
			if !ok {
				return fmt.Errorf("original node %q isn't valid node", h.Linkname)
			}
			n.attr.Nlink++
			// We register the target node as name "base". When we query this hardlink node,
			// we can easily get the target name by seeing target's fullname field.
			if ok := parentDir.AddChild(base, target, true); !ok {
				return fmt.Errorf("failed to add child %q", base)
			}
		default:
			// Normal node so simply create it.
			attr, sAttr := headerToAttr(h)
			if ok := parentDir.AddChild(base, n.NewPersistentInode(ctx, &node{
				fullname: fullname,
				attr:     attr,
				r:        io.NewSectionReader(n.tr, pw.CurrentPos(), h.Size),
				link:     h.Linkname,
				xattr:    xattrs,
				tr:       n.tr,
				monitor:  n.monitor,
			}, sAttr), true); !ok {
				return fmt.Errorf("failed to add child %q", base)
			}
		}

		continue
	}

	// Add whiteout nodes if necessary. If an entry exists as both of a
	// whiteout file and a normal entry, we simply prioritize the normal entry.
	for _, w := range whiteouts {
		dir, base := filepath.Split(w)
		if _, err := n.walkDown(filepath.Join(dir, base[len(whiteoutPrefix):]), getnode); err != nil {
			p, err := n.walkDown(dir, getnode)
			if err != nil {
				return errors.Wrapf(err, "parent node of whiteout %q is not found", w)
			}
			if ok := p.AddChild(base[len(whiteoutPrefix):], n.NewPersistentInode(ctx, &whiteout{}, fusefs.StableAttr{Mode: syscall.S_IFCHR}), true); !ok {
				return fmt.Errorf("failed to add child %q", base)
			}
		}
	}

	return nil
}

type walkFunc func(n *fusefs.Inode, base string) (*fusefs.Inode, error)

func (n *node) walkDown(path string, walkFn walkFunc) (ino *fusefs.Inode, err error) {
	ino = n.Root()
	for _, comp := range strings.Split(path, "/") {
		if len(comp) == 0 {
			continue
		}
		if ino == nil {
			return nil, fmt.Errorf("corresponding node of %q is not found", comp)
		}
		if ino, err = walkFn(ino, comp); err != nil {
			return nil, err
		}
	}
	return
}

type whiteout struct {
	fusefs.Inode
}

var _ = (fusefs.NodeGetattrer)((*whiteout)(nil))

func (w *whiteout) Getattr(ctx context.Context, f fusefs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Ino = 0 // TODO
	out.Size = 0
	out.Blksize = uint32(DefaultBlockSize)
	out.Blocks = 0
	out.Mode = syscall.S_IFCHR
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	out.Rdev = uint32(unix.Mkdev(0, 0))
	out.Nlink = 1
	out.Padding = 0 // TODO
	return 0
}

// monitor
type Monitor interface {
	OnLookup(name string)
	OnReadlink(name, linkname string)
	OnOpen(name string)
	OnRead(name string, off, size int64)
	OnGetAttr(name string)
	DumpLog() []string
}

func NewOpenReadMonitor() Monitor {
	return &OpenReadMonitor{}
}

type OpenReadMonitor struct {
	log   []string
	logMu sync.Mutex
}

func (m *OpenReadMonitor) OnOpen(name string) {
	m.logMu.Lock()
	m.log = append(m.log, name)
	m.logMu.Unlock()
}

func (m *OpenReadMonitor) OnRead(name string, off, size int64) {
	m.logMu.Lock()
	m.log = append(m.log, name)
	m.logMu.Unlock()
}

func (m *OpenReadMonitor) DumpLog() []string {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	return m.log
}

func (m *OpenReadMonitor) OnLookup(name string)             {}
func (m *OpenReadMonitor) OnReadlink(name, linkname string) {}
func (m *OpenReadMonitor) OnGetAttr(name string)            {}

// Utilities
func override(a, b map[string][]byte) map[string][]byte {
	if b == nil {
		return a
	}
	if a == nil {
		return b
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

func headerToAttr(h *tar.Header) (fuse.Attr, fusefs.StableAttr) {
	out := fuse.Attr{}

	out.Size = uint64(h.Size)
	out.Blocks = uint64(h.Size/DefaultBlockSize + 1)
	out.Atime = uint64(h.AccessTime.Unix())
	out.Mtime = uint64(h.ModTime.Unix())
	out.Ctime = uint64(h.ChangeTime.Unix())
	out.Atimensec = uint32(h.AccessTime.UnixNano())
	out.Mtimensec = uint32(h.ModTime.UnixNano())
	out.Ctimensec = uint32(h.ChangeTime.UnixNano())
	out.Mode = fileModeToSystemMode(h.FileInfo().Mode())
	out.Blksize = uint32(DefaultBlockSize)
	out.Owner = fuse.Owner{Uid: uint32(h.Uid), Gid: uint32(h.Gid)}
	out.Rdev = uint32(unix.Mkdev(uint32(h.Devmajor), uint32(h.Devminor)))

	// ad-hoc
	out.Ino = 0
	out.Nlink = 1
	out.Padding = 0

	return out, fusefs.StableAttr{
		Mode: out.Mode,
		// We don't care about inode and generation and let it be
		// managed by go-fuse.
	}
}

func copyAttr(dest *fuse.Attr, src fuse.Attr) {
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

func fileModeToSystemMode(m os.FileMode) uint32 {

	// Convert os.FileMode to system's native bitmap.
	sm := uint32(m & 0777)
	switch m & os.ModeType {
	case os.ModeDevice:
		sm |= syscall.S_IFBLK
	case os.ModeDevice | os.ModeCharDevice:
		sm |= syscall.S_IFCHR
	case os.ModeDir:
		sm |= syscall.S_IFDIR
	case os.ModeNamedPipe:
		sm |= syscall.S_IFIFO
	case os.ModeSymlink:
		sm |= syscall.S_IFLNK
	case os.ModeSocket:
		sm |= syscall.S_IFSOCK
	default: // regular file.
		sm |= syscall.S_IFREG
	}
	if m&os.ModeSetgid != 0 {
		sm |= syscall.S_ISGID
	}
	if m&os.ModeSetuid != 0 {
		sm |= syscall.S_ISUID
	}
	if m&os.ModeSticky != 0 {
		sm |= syscall.S_ISVTX
	}

	return sm
}
