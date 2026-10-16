//go:build fuse

// Package fusefs presents a vfs.FS as a read-only FUSE filesystem: the
// virtualized checkout behind `kudzu mount`. Directories are listed from
// tree objects and files read from blobs, each fetched the first time it is
// needed. It is only built with -tags fuse, so the default kudzu binary
// does not depend on FUSE.
package fusefs

import (
	"context"
	"errors"
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

// Mount serves v at dir until the returned server is unmounted. A revision
// never changes, so the kernel may cache entries, attributes and file
// contents for as long as it likes.
func Mount(dir string, v *vfs.FS, logger *log.Logger) (*fuse.Server, error) {
	forever := 24 * time.Hour
	return gofs.Mount(dir, &dirNode{node: node{v: v, n: v.Root(), log: logger}}, &gofs.Options{
		EntryTimeout:    &forever,
		AttrTimeout:     &forever,
		NegativeTimeout: &forever,
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
		MountOptions: fuse.MountOptions{
			FsName:      "kudzu:" + v.Changeset.Short(),
			Name:        "kudzu",
			Options:     []string{"ro"},
			DirectMount: true, // mount(2) directly when allowed, else fusermount
		},
	})
}

// node is what every kind of inode knows; each kind embeds its own Inode.
type node struct {
	v   *vfs.FS
	n   vfs.Node
	log *log.Logger
}

type dirNode struct {
	gofs.Inode
	node
}

type fileNode struct {
	gofs.Inode
	node
}

type linkNode struct {
	gofs.Inode
	node
}

var (
	_ gofs.NodeLookuper   = (*dirNode)(nil)
	_ gofs.NodeReaddirer  = (*dirNode)(nil)
	_ gofs.NodeGetattrer  = (*dirNode)(nil)
	_ gofs.NodeOpener     = (*fileNode)(nil)
	_ gofs.NodeGetattrer  = (*fileNode)(nil)
	_ gofs.FileReader     = (*fileHandle)(nil)
	_ gofs.FileReleaser   = (*fileHandle)(nil)
	_ gofs.NodeReadlinker = (*linkNode)(nil)
	_ gofs.NodeGetattrer  = (*linkNode)(nil)
)

func typeBits(n vfs.Node) uint32 {
	switch n.Mode {
	case object.ModeDir:
		return syscall.S_IFDIR
	case object.ModeSymlink:
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

func (nd *node) fill(a *fuse.Attr) {
	a.Mode = typeBits(nd.n)
	switch nd.n.Mode {
	case object.ModeDir, object.ModeExec:
		a.Mode |= 0o555
	case object.ModeSymlink:
		a.Mode |= 0o777
	default:
		a.Mode |= 0o444
	}
	a.Size = uint64(nd.n.Size)
	a.Mtime, a.Ctime, a.Atime = uint64(nd.v.Time), uint64(nd.v.Time), uint64(nd.v.Time)
	a.Blocks = (a.Size + 511) / 512
	a.Nlink = 1
}

func (nd *node) errno(err error) syscall.Errno {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	}
	if nd.log != nil {
		nd.log.Printf("%s: %v", nd.n.Path, err)
	}
	return syscall.EIO
}

func (d *dirNode) Getattr(_ context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	d.fill(&out.Attr)
	return 0
}

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	n, err := d.v.Lookup(ctx, d.n, name)
	if err != nil {
		return nil, d.errno(err)
	}
	child := node{v: d.v, n: n, log: d.log}
	var emb gofs.InodeEmbedder
	switch n.Mode {
	case object.ModeDir:
		emb = &dirNode{node: child}
	case object.ModeSymlink:
		emb = &linkNode{node: child}
	default:
		emb = &fileNode{node: child}
	}
	child.fill(&out.Attr)
	return d.NewInode(ctx, emb, gofs.StableAttr{Mode: typeBits(n)}), 0
}

func (d *dirNode) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	nodes, err := d.v.ReadDir(ctx, d.n)
	if err != nil {
		return nil, d.errno(err)
	}
	entries := make([]fuse.DirEntry, len(nodes))
	for i, n := range nodes {
		entries[i] = fuse.DirEntry{Name: path.Base(n.Path), Mode: typeBits(n)}
	}
	return gofs.NewListDirStream(entries), 0
}

func (f *fileNode) Getattr(_ context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	f.fill(&out.Attr)
	return 0
}

func (f *fileNode) Open(_ context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
		return nil, 0, syscall.EROFS
	}
	// Contents never change, so the page cache stays valid across opens.
	return &fileHandle{h: f.v.OpenFile(f.n), node: &f.node}, fuse.FOPEN_KEEP_CACHE, 0
}

// fileHandle serves an open file's reads from content fetched once.
type fileHandle struct {
	h    *vfs.Handle
	node *node
}

func (fh *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := fh.h.ReadAt(ctx, dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fh.node.errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (fh *fileHandle) Release(context.Context) syscall.Errno {
	fh.h.Close()
	return 0
}

func (l *linkNode) Getattr(_ context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	l.fill(&out.Attr)
	return 0
}

func (l *linkNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := l.v.ReadFile(ctx, l.n)
	if err != nil {
		return nil, l.errno(err)
	}
	return target, 0
}
