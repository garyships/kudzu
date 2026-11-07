// Package client implements the working copy (the article's scn client). The
// client is deliberately thin: it keeps a bounded object cache, lays out the
// files of a (sparse) checkout, and notices local edits. History, blame,
// merges, rebases and commits all happen on the server.
package client

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
)

const metaDir = ".kudzu"

// DefaultCacheLimit bounds the local object cache.
const DefaultCacheLimit = 2 << 30

type State struct {
	Remote     string      `json:"remote"`
	Changeset  object.ID   `json:"changeset"`
	Tree       object.ID   `json:"tree"`
	Sparse     sparse.Spec `json:"sparse,omitempty"`
	CacheLimit int64       `json:"cache_limit"`
	// CacheBlobs keeps fetched file contents in the local object cache. It is
	// off when the remote is a node daemon, which already caches them for
	// every job on the machine; trees are always cached locally.
	CacheBlobs bool `json:"cache_blobs"`
}

// IndexEntry records what was materialized at a path, plus the stat data
// used to skip re-hashing unchanged files.
type IndexEntry struct {
	Mode    uint32
	Blob    object.ID
	Size    int64
	MtimeNs int64
}

type WC struct {
	Root   string
	State  State
	Index  map[string]IndexEntry
	Remote *proto.Remote
	Objs   *store.FS
	Out    io.Writer
}

// Open finds the working copy containing dir: the nearest .kudzu above it.
// vfs.FindMount stops at the same one, so of a writable mount and a working
// copy, the nearer one is always the one used.
func Open(dir string) (*WC, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if fi, err := os.Lstat(filepath.Join(d, metaDir)); err == nil {
			meta, err := openMeta(d, fi)
			if err != nil {
				return nil, err
			}
			defer meta.Close()
			return load(d, meta)
		}
		if filepath.Dir(d) == d {
			return nil, fmt.Errorf("not inside a kudzu working copy: %s", abs)
		}
	}
}

// openMeta opens root's .kudzu directory, which Lstat described as fi, if
// checkOwner trusts it. It must be a real directory, not a symlink, and the
// state that names the remote is read through the directory that was
// checked: a path looked up again could have been redirected in between.
func openMeta(root string, fi os.FileInfo) (*os.Root, error) {
	path := filepath.Join(root, metaDir)
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing the working copy at %s: %s is a symlink, not a directory", root, path)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("refusing the working copy at %s: %s is not a directory", root, path)
	}
	if err := checkOwner(root, fi); err != nil {
		return nil, err
	}
	meta, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if st, err := meta.Stat("."); err != nil || !os.SameFile(fi, st) {
		meta.Close()
		return nil, fmt.Errorf("refusing the working copy at %s: %s changed while it was being opened", root, path)
	}
	return meta, nil
}

// metaErr makes the path in an error from meta, which is relative to it,
// a full one.
func metaErr(meta *os.Root, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) && !filepath.IsAbs(pe.Path) {
		pe.Path = filepath.Join(meta.Name(), pe.Path)
	}
	return err
}

func load(root string, meta *os.Root) (*WC, error) {
	w := &WC{Root: root, Out: io.Discard}
	b, err := meta.ReadFile("state.json")
	if err != nil {
		return nil, metaErr(meta, err)
	}
	if err := json.Unmarshal(b, &w.State); err != nil {
		return nil, fmt.Errorf("corrupt working copy state: %w", err)
	}
	f, err := meta.Open("index")
	if err != nil {
		return nil, metaErr(meta, err)
	}
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&w.Index); err != nil {
		return nil, fmt.Errorf("corrupt working copy index: %w", err)
	}
	if w.Remote, err = proto.Dial(w.State.Remote); err != nil {
		return nil, err
	}
	if w.Objs, err = store.OpenFS(filepath.Join(root, metaDir, "objects"), w.State.CacheLimit); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *WC) Save() error {
	dir := filepath.Join(w.Root, metaDir)
	b, err := json.MarshalIndent(w.State, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "state.json"), b); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "index.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(w.Index); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "index"))
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Clone creates a working copy of rev in dir, fetching only the trees and
// blobs that the sparse spec selects.
func Clone(ctx context.Context, remote, dir, rev string, sp sparse.Spec, out io.Writer) (*WC, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, metaDir), 0o755); err != nil {
		return nil, err
	}
	w := &WC{Root: root, Index: map[string]IndexEntry{}, Out: out}
	w.State = State{Remote: remote, Sparse: sp, CacheLimit: DefaultCacheLimit, CacheBlobs: !strings.HasPrefix(remote, "unix://")}
	if w.Remote, err = proto.Dial(remote); err != nil {
		return nil, err
	}
	if w.Objs, err = store.OpenFS(filepath.Join(root, metaDir, "objects"), w.State.CacheLimit); err != nil {
		return nil, err
	}
	res, err := w.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	files, err := w.collectFiles(ctx, res.Tree, sp)
	if err != nil {
		return nil, err
	}
	if err := w.apply(ctx, files, nil, false); err != nil {
		return nil, err
	}
	w.State.Changeset, w.State.Tree = res.Changeset, res.Tree
	return w, w.Save()
}

type resolved struct {
	Changeset object.ID `json:"changeset"`
	Tree      object.ID `json:"tree"`
}

func (w *WC) resolve(ctx context.Context, rev string) (resolved, error) {
	var r resolved
	err := w.Remote.GetJSON(ctx, "/api/resolve", map[string][]string{"rev": {rev}}, &r)
	return r, err
}

func (w *WC) abs(p string) string { return filepath.Join(w.Root, filepath.FromSlash(p)) }
