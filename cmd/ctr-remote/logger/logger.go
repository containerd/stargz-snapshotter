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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/stargz-snapshotter/cmd/ctr-remote/util"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
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
	conn := nodefs.NewFileSystemConnector(root, &nodefs.Options{
		NegativeTimeout: 0,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
		Owner:           nil, // preserve owners.
	})
	server, err := fuse.NewServer(conn.RawFS(), mountPoint, &fuse.MountOptions{AllowOther: true})
	if err != nil {
		return nil, fmt.Errorf("Error: Failed failed: %v", err)
	}
	if err := root.InitNodes(); err != nil {
		return nil, fmt.Errorf("Error: Failed to initialize nodes: %v", err)
	}
	go server.Serve()
	if err := server.WaitMount(); err != nil {
		return nil, fmt.Errorf("Error: Failed to mount filesystem: %v", err)
	}

	return func() error { return server.Unmount() }, nil
}

// Nodes
func newRoot(tarfile io.ReaderAt, monitor Monitor) *node {
	root := &node{
		Node:    nodefs.NewDefaultNode(),
		tr:      tarfile,
		monitor: monitor,
	}
	root.root = root
	now := time.Now()
	root.attr.SetTimes(&now, &now, &now)
	root.attr.Mode = fuse.S_IFDIR | 0777

	return root
}

type node struct {
	nodefs.Node // Embedded default node.

	fullname string
	attr     fuse.Attr
	r        io.ReaderAt
	link     string // Symbolic link.
	xattr    map[string][]byte

	root *node
	tr   io.ReaderAt

	monitor Monitor
}

func (n *node) OnMount(conn *nodefs.FileSystemConnector) {
	n.monitor.OnMount()
}

func (n *node) OnUnmount() {
	n.monitor.OnUnmount()
}

func (n *node) Lookup(out *fuse.Attr, name string, context *fuse.Context) (*nodefs.Inode, fuse.Status) {
	c := n.Inode().GetChild(name)
	if c == nil {
		return nil, fuse.ENOENT
	}
	if s := c.Node().GetAttr(out, nil, context); s != fuse.OK {
		return nil, s
	}
	n.monitor.OnLookup(n.fullname)

	return c, fuse.OK
}

func (n *node) Deletable() bool {
	// Undeletable because of read-only filesystem
	return false
}

func (n *node) OnForget() {
	// Do nothing because the node is undeletable.
	n.monitor.OnForget(n.fullname)
}

func (n *node) Access(mode uint32, context *fuse.Context) (code fuse.Status) {
	if context.Owner.Uid == 0 { // root can do anything.
		return fuse.OK
	}
	if mode == 0 { // Requires nothing.
		return fuse.OK
	}
	var attr fuse.Attr
	s := n.GetAttr(&attr, nil, context)
	if s != fuse.OK {
		return s
	}
	var shift uint32
	if attr.Owner.Uid == context.Owner.Uid {
		shift = 6
	} else if attr.Owner.Gid == context.Owner.Gid {
		shift = 3
	} else {
		shift = 0
	}
	if mode<<shift&attr.Mode != 0 {
		n.monitor.OnAccess(n.fullname)
		return fuse.OK
	}

	return fuse.EPERM
}

func (n *node) Readlink(c *fuse.Context) ([]byte, fuse.Status) {
	n.monitor.OnReadlink(n.fullname, n.link)
	return []byte(n.link), fuse.OK
}

func (n *node) Open(flags uint32, context *fuse.Context) (file nodefs.File, code fuse.Status) {
	n.monitor.OnOpen(n.fullname)
	// It is OK to return (nil, OK) here. In that case,
	// the Node need to implement "Read" function.
	return nil, fuse.OK
}

func (n *node) Read(file nodefs.File, dest []byte, off int64, context *fuse.Context) (fuse.ReadResult, fuse.Status) {
	if _, err := n.r.ReadAt(dest, off); err != nil && err != io.EOF {
		fmt.Printf("Error: Failed to read %s: %v", n.fullname, err)
		return nil, fuse.EIO
	}
	n.monitor.OnRead(n.fullname, off, int64(len(dest)))
	return fuse.ReadResultData(dest), fuse.OK
}

func (n *node) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) (code fuse.Status) {
	n.dumpAttr(out)
	n.monitor.OnGetAttr(n.fullname)
	return fuse.OK
}

func (n *node) GetXAttr(attribute string, context *fuse.Context) (data []byte, code fuse.Status) {
	if v, ok := n.xattr[attribute]; ok {
		return v, fuse.OK
	}
	return nil, fuse.ENOATTR
}

func (n *node) ListXAttr(ctx *fuse.Context) (attrs []string, code fuse.Status) {
	for k := range n.xattr {
		attrs = append(attrs, k)
	}
	return attrs, fuse.OK
}

func (n *node) StatFs() *fuse.StatfsOut {
	// http://man7.org/linux/man-pages/man2/statfs.2.html
	return &fuse.StatfsOut{
		Blocks:  0, // dummy
		Bfree:   0,
		Bavail:  0,
		Files:   0, // dummy
		Ffree:   0,
		Bsize:   0, // dummy
		NameLen: 1<<32 - 1,
		Frsize:  0, // dummy
		Padding: 0,
		Spare:   [6]uint32{},
	}
}

func (n *node) dumpAttr(out *fuse.Attr) {
	out.Ino = n.attr.Ino
	out.Size = n.attr.Size
	out.Blocks = n.attr.Blocks
	out.Atime = n.attr.Atime
	out.Mtime = n.attr.Mtime
	out.Ctime = n.attr.Ctime
	out.Atimensec = n.attr.Atimensec
	out.Mtimensec = n.attr.Mtimensec
	out.Ctimensec = n.attr.Ctimensec
	out.Mode = n.attr.Mode
	out.Nlink = n.attr.Nlink
	out.Owner = n.attr.Owner
	out.Rdev = n.attr.Rdev
	out.Blksize = n.attr.Blksize
	out.Padding = n.attr.Padding
}

func (n *node) headerToAttr(h *tar.Header) fuse.Attr {
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

	return out
}

func (n *node) InitNodes() error {
	pw, err := util.NewPositionWatcher(n.tr)
	if err != nil {
		return fmt.Errorf("Failed to make position watcher: %v", err)
	}
	tr := tar.NewReader(pw)

	// Walk functions for nodes
	getormake := func(n *nodefs.Inode, base string) (c *nodefs.Inode, err error) {
		if c = n.GetChild(base); c == nil {
			// Make temporary dummy node.
			c = n.NewChild(base, true, &node{Node: nodefs.NewDefaultNode()})
		}
		return
	}
	getnode := func(n *nodefs.Inode, base string) (c *nodefs.Inode, err error) {
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
				return fmt.Errorf("Failed to parse tar file: %v", err)
			}
			break
		}
		var (
			fullname     = filepath.Clean(h.Name)
			dir, base    = filepath.Split(fullname)
			parentDir    *nodefs.Inode
			existingNode *nodefs.Inode
			xattrs       = make(map[string][]byte)
		)
		if parentDir, err = n.walkDown(dir, getormake); err != nil {
			return fmt.Errorf("failed to make dummy nodes: %v", err)
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
				return fmt.Errorf("node \"%s\" is placeholder but not a directory",
					fullname)
			}
			// This is "placeholder node" which has been registered in previous loop as
			// an intermediate directory. Now we update it with real one.
			ph, ok := existingNode.Node().(*node)
			if !ok {
				return fmt.Errorf("invalid placeholder node %q", fullname)
			}
			ph.Node = nodefs.NewDefaultNode()
			ph.fullname = fullname
			ph.attr = n.headerToAttr(h)
			ph.link = h.Linkname
			ph.xattr = overrideXattr(ph.xattr, xattrs) // preserve previously assigned xattr
			ph.root = n.root
			ph.tr = n.tr
			ph.monitor = n.monitor
		case strings.HasPrefix(base, whiteoutPrefix):
			if base == whiteoutOpaqueDir {
				// This node is opaque directory indicator so we append opaque xattr to
				// the parent directory.
				pn, ok := parentDir.Node().(*node)
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
				return fmt.Errorf("Linkname of hardlink %s is not found", fullname)
			}
			// This node is a hardlink. Same as major tar tools(GNU tar etc.),
			// we pretend that the target node of this hard link has already been appeared.
			target, err := n.walkDown(filepath.Clean(h.Linkname), getnode)
			if err != nil {
				return fmt.Errorf("hardlink(%q ==> %q) is not found: %v",
					fullname, h.Linkname, err)
			}
			n, ok := target.Node().(*node)
			if !ok {
				return fmt.Errorf("original node %q isn't valid node", h.Linkname)
			}
			n.attr.Nlink++
			// We register the target node as name "base". When we query this hardlink node,
			// we can easily get the target name by seeing target's fullname field.
			parentDir.AddChild(base, target)
		default:
			// Normal node so simply create it.
			_ = parentDir.NewChild(base, h.FileInfo().IsDir(), &node{
				Node:     nodefs.NewDefaultNode(),
				fullname: fullname,
				attr:     n.headerToAttr(h),
				r:        io.NewSectionReader(n.tr, pw.CurrentPos(), h.Size),
				link:     h.Linkname,
				xattr:    xattrs,
				root:     n.root,
				tr:       n.tr,
				monitor:  n.monitor,
			})
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
				return fmt.Errorf("parent node of whiteout %q is not found", w)
			}
			p.NewChild(base[len(whiteoutPrefix):], false, &whiteout{nodefs.NewDefaultNode()})
		}
	}

	return nil
}

type walkFunc func(n *nodefs.Inode, base string) (*nodefs.Inode, error)

func (n *node) walkDown(path string, walkFn walkFunc) (ino *nodefs.Inode, err error) {
	ino = n.root.Inode()
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
	nodefs.Node
}

func (w *whiteout) GetAttr(out *fuse.Attr, file nodefs.File, context *fuse.Context) (code fuse.Status) {
	out.Ino = 0 // TODO
	out.Size = 0
	out.Blksize = uint32(DefaultBlockSize)
	out.Blocks = 0
	out.Mode = syscall.S_IFCHR
	out.Owner = fuse.Owner{Uid: 0, Gid: 0}
	out.Rdev = uint32(unix.Mkdev(0, 0))
	out.Nlink = 1
	out.Padding = 0 // TODO
	return fuse.OK
}

// monitor
type Monitor interface {
	OnMount()
	OnUnmount()
	OnLookup(name string)
	OnForget(name string)
	OnAccess(name string)
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
	log []string
}

func (m *OpenReadMonitor) OnOpen(name string) {
	m.log = append(m.log, name)
}

func (m *OpenReadMonitor) OnRead(name string, off, size int64) {
	m.log = append(m.log, name)
}

func (m *OpenReadMonitor) DumpLog() []string {
	return m.log
}

func (m *OpenReadMonitor) OnMount() {}

func (m *OpenReadMonitor) OnUnmount() {}

func (m *OpenReadMonitor) OnLookup(name string) {}

func (m *OpenReadMonitor) OnForget(name string) {}

func (m *OpenReadMonitor) OnAccess(name string) {}

func (m *OpenReadMonitor) OnReadlink(name, linkname string) {}

func (m *OpenReadMonitor) OnGetAttr(name string) {}

// Utilities
func overrideXattr(a, b map[string][]byte) map[string][]byte {
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
