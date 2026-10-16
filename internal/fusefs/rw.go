//go:build fuse

package fusefs

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/vfs"
)

// MountWritable serves an overlay at dir: files read from the base revision
// are fetched on first read, as in Mount, and everything written goes to the
// overlay. The root also shows .kudzu/mount.json, a read-only file telling
// the kudzu command where the overlay's control socket is, so that status,
// diff, commit and update work from inside the mount.
//
// Unlike a read-only mount, entries and attributes are cached for only a
// second: a commit or update replaces the base.
func MountWritable(dir string, o *vfs.Overlay, info vfs.MountInfo, logger *log.Logger) (*fuse.Server, error) {
	sec := time.Second
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, err
	}
	root := &rwNode{o: o, log: logger, info: append(data, '\n')}
	return gofs.Mount(dir, root, &gofs.Options{
		EntryTimeout:    &sec,
		AttrTimeout:     &sec,
		NegativeTimeout: &sec,
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		MountOptions: fuse.MountOptions{
			FsName:      "kudzu:" + o.Base().Changeset.Short(),
			Name:        "kudzu",
			DirectMount: true,
			Logger:      quiet,
		},
	})
}

// rwNode is any file, directory or symlink of a writable mount. It finds
// its path from its place in the inode tree, which go-fuse keeps up to date
// across renames.
type rwNode struct {
	gofs.Inode
	o     *vfs.Overlay
	log   *log.Logger
	info  []byte      // root only: .kudzu/mount.json
	kudzu *gofs.Inode // root only: the .kudzu directory
}

var (
	_ gofs.NodeLookuper   = (*rwNode)(nil)
	_ gofs.NodeReaddirer  = (*rwNode)(nil)
	_ gofs.NodeGetattrer  = (*rwNode)(nil)
	_ gofs.NodeSetattrer  = (*rwNode)(nil)
	_ gofs.NodeOpener     = (*rwNode)(nil)
	_ gofs.NodeCreater    = (*rwNode)(nil)
	_ gofs.NodeMkdirer    = (*rwNode)(nil)
	_ gofs.NodeUnlinker   = (*rwNode)(nil)
	_ gofs.NodeRmdirer    = (*rwNode)(nil)
	_ gofs.NodeRenamer    = (*rwNode)(nil)
	_ gofs.NodeSymlinker  = (*rwNode)(nil)
	_ gofs.NodeReadlinker = (*rwNode)(nil)
	_ gofs.FileReader     = (*rwHandle)(nil)
	_ gofs.FileWriter     = (*rwHandle)(nil)
	_ gofs.FileFsyncer    = (*rwHandle)(nil)
	_ gofs.FileFlusher    = (*rwHandle)(nil)
	_ gofs.FileGetattrer  = (*rwHandle)(nil)
	_ gofs.FileReleaser   = (*rwHandle)(nil)
)

func (n *rwNode) path() string { return n.Path(nil) }

func (n *rwNode) child(name string) string { return object.JoinPath(n.path(), name) }

func entryType(mode uint32) uint32 {
	switch mode {
	case object.ModeDir:
		return syscall.S_IFDIR
	case object.ModeSymlink:
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

func (n *rwNode) fill(e vfs.Entry, a *fuse.Attr) {
	a.Mode = entryType(e.Mode)
	switch e.Mode {
	case object.ModeDir, object.ModeExec:
		a.Mode |= 0o755
	case object.ModeSymlink:
		a.Mode |= 0o777
	default:
		a.Mode |= 0o644
	}
	a.Size = uint64(e.Size)
	a.Blocks = (a.Size + 511) / 512
	a.Nlink = 1
	if !e.Upper {
		t := uint64(n.o.Base().Time)
		a.Mtime, a.Ctime, a.Atime = t, t, t
	}
}

// inode returns the inode for an entry. Inode numbers come from the path
// and type, so a path keeps its number between lookups.
func (n *rwNode) inode(ctx context.Context, e vfs.Entry) *gofs.Inode {
	h := fnv.New64a()
	io.WriteString(h, e.Path)
	h.Write([]byte{0, byte(entryType(e.Mode) >> 12)})
	ino := h.Sum64() | 1<<63 // stay clear of the root's number
	return n.NewInode(ctx, &rwNode{o: n.o, log: n.log}, gofs.StableAttr{Mode: entryType(e.Mode), Ino: ino})
}

func (n *rwNode) errno(err error) syscall.Errno {
	var en syscall.Errno
	switch {
	case err == nil:
		return 0
	case errors.Is(err, fs.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, fs.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, vfs.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, vfs.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, vfs.ErrNotEmpty):
		return syscall.ENOTEMPTY
	case errors.Is(err, vfs.ErrCrossDir):
		return syscall.EXDEV
	case errors.Is(err, vfs.ErrInvalid):
		return syscall.EINVAL
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.As(err, &en):
		return en
	}
	if n.log != nil {
		n.log.Printf("%s: %v", n.path(), err)
	}
	return syscall.EIO
}

func (n *rwNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	if n.info != nil && name == ".kudzu" {
		return n.kudzuDir(ctx), 0
	}
	e, err := n.o.Stat(ctx, n.child(name))
	if err != nil {
		return nil, n.errno(err)
	}
	n.fill(e, &out.Attr)
	return n.inode(ctx, e), 0
}

// kudzuDir is the root's read-only .kudzu directory with mount.json.
func (n *rwNode) kudzuDir(ctx context.Context) *gofs.Inode {
	if n.kudzu == nil {
		dir := n.NewPersistentInode(ctx, &gofs.Inode{}, gofs.StableAttr{Mode: syscall.S_IFDIR})
		// Owned by whoever mounted it: the kudzu command only trusts a
		// mount.json that belongs to the user running it.
		owner := fuse.Owner{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
		file := &gofs.MemRegularFile{Data: n.info, Attr: fuse.Attr{Mode: 0o444, Owner: owner}}
		dir.AddChild("mount.json", n.NewPersistentInode(ctx, file, gofs.StableAttr{}), false)
		n.kudzu = dir
	}
	return n.kudzu
}

func (n *rwNode) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	ents, err := n.o.ReadDir(ctx, n.path())
	if err != nil {
		return nil, n.errno(err)
	}
	out := make([]fuse.DirEntry, len(ents))
	for i, e := range ents {
		out[i] = fuse.DirEntry{Name: path.Base(e.Path), Mode: entryType(e.Mode)}
	}
	return gofs.NewListDirStream(out), 0
}

func (n *rwNode) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if h, ok := fh.(*rwHandle); ok && h.w != nil {
		return h.Getattr(ctx, out)
	}
	e, err := n.o.Stat(ctx, n.path())
	if err != nil {
		return n.errno(err)
	}
	n.fill(e, &out.Attr)
	return 0
}

func (n *rwNode) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	p := n.path()
	if size, ok := in.GetSize(); ok {
		var err error
		if h, isRW := fh.(*rwHandle); isRW && h.w != nil {
			err = h.w.Truncate(int64(size))
		} else {
			err = n.o.Truncate(ctx, p, int64(size))
		}
		if err != nil {
			return n.errno(err)
		}
	}
	if mode, ok := in.GetMode(); ok {
		if err := n.o.SetMode(ctx, p, mode&0o111 != 0); err != nil {
			return n.errno(err)
		}
	}
	// Times are not versioned; accept and ignore them, so touch works.
	return n.Getattr(ctx, fh, out)
}

const writeFlags = syscall.O_WRONLY | syscall.O_RDWR | syscall.O_TRUNC | syscall.O_APPEND

func (n *rwNode) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	p := n.path()
	if flags&writeFlags != 0 {
		f, err := n.o.OpenWrite(ctx, p, false, false, flags&syscall.O_TRUNC != 0, false)
		if err != nil {
			return nil, 0, n.errno(err)
		}
		return &rwHandle{o: n.o, w: f}, 0, 0
	}
	r, err := n.o.OpenRead(ctx, p)
	if err != nil {
		return nil, 0, n.errno(err)
	}
	return &rwHandle{o: n.o, r: r}, 0, 0
}

func (n *rwNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	p := n.child(name)
	f, err := n.o.OpenWrite(ctx, p, true, flags&syscall.O_EXCL != 0, flags&syscall.O_TRUNC != 0, mode&0o111 != 0)
	if err != nil {
		return nil, nil, 0, n.errno(err)
	}
	e, err := n.o.Stat(ctx, p)
	if err != nil {
		n.o.CloseWrite(f)
		return nil, nil, 0, n.errno(err)
	}
	n.fill(e, &out.Attr)
	return n.inode(ctx, e), &rwHandle{o: n.o, w: f}, 0, 0
}

func (n *rwNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p := n.child(name)
	if err := n.o.Mkdir(ctx, p); err != nil {
		return nil, n.errno(err)
	}
	e, err := n.o.Stat(ctx, p)
	if err != nil {
		return nil, n.errno(err)
	}
	n.fill(e, &out.Attr)
	return n.inode(ctx, e), 0
}

func (n *rwNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p := n.child(name)
	if err := n.o.Symlink(ctx, target, p); err != nil {
		return nil, n.errno(err)
	}
	e, err := n.o.Stat(ctx, p)
	if err != nil {
		return nil, n.errno(err)
	}
	n.fill(e, &out.Attr)
	return n.inode(ctx, e), 0
}

func (n *rwNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.o.ReadFile(ctx, n.path())
	if err != nil {
		return nil, n.errno(err)
	}
	return target, 0
}

func (n *rwNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return n.errno(n.o.Remove(ctx, n.child(name)))
}

func (n *rwNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return n.errno(n.o.Rmdir(ctx, n.child(name)))
}

const renameNoReplace = 1 // RENAME_NOREPLACE

func (n *rwNode) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	to := object.JoinPath(newParent.EmbeddedInode().Path(nil), newName)
	switch flags {
	case 0:
	case renameNoReplace:
		if _, err := n.o.Stat(ctx, to); err == nil {
			return syscall.EEXIST
		}
	default:
		return syscall.EINVAL // RENAME_EXCHANGE and friends
	}
	return n.errno(n.o.Rename(ctx, n.child(name), to))
}

// rwHandle is an open file: w for one opened for writing (in the overlay),
// r for one opened for reading.
type rwHandle struct {
	o *vfs.Overlay
	w *os.File
	r vfs.FileReader
}

func (h *rwHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	var k int
	var err error
	if h.w != nil {
		k, err = h.w.ReadAt(dest, off)
	} else {
		k, err = h.r.ReadAt(ctx, dest, off)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:k]), 0
}

func (h *rwHandle) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if h.w == nil {
		return 0, syscall.EBADF
	}
	k, err := h.w.WriteAt(data, off)
	if err != nil {
		return uint32(k), syscall.EIO
	}
	return uint32(k), 0
}

func (h *rwHandle) Getattr(_ context.Context, out *fuse.AttrOut) syscall.Errno {
	if h.w == nil {
		return syscall.ENOSYS // the node answers instead
	}
	info, err := h.w.Stat()
	if err != nil {
		return syscall.EIO
	}
	out.Mode = syscall.S_IFREG | uint32(info.Mode().Perm())
	out.Size = uint64(info.Size())
	out.Blocks = (out.Size + 511) / 512
	out.Nlink = 1
	t := uint64(info.ModTime().Unix())
	out.Mtime, out.Ctime, out.Atime = t, t, t
	return 0
}

func (h *rwHandle) Flush(context.Context) syscall.Errno { return 0 }

func (h *rwHandle) Fsync(context.Context, uint32) syscall.Errno {
	if h.w != nil && h.w.Sync() != nil {
		return syscall.EIO
	}
	return 0
}

func (h *rwHandle) Release(context.Context) syscall.Errno {
	if h.w != nil {
		h.o.CloseWrite(h.w)
	} else {
		h.r.Close()
	}
	return 0
}
