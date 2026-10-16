// Package vfs is a lazily materialized, read-only view of one revision: the
// core of `kudzu mount`. Nothing is downloaded up front. A directory's tree
// object is fetched the first time the directory is listed or searched, and
// a file's blob the first time the file is read, both by ID through the
// object protocol (normally from the node daemon), and both are verified and
// kept in a local cache. File sizes come from the server in one request per
// directory, so stat and ls -l are exact without fetching any contents.
//
// The package has no FUSE dependency; internal/fusefs adapts it to a
// filesystem, and tests drive it directly.
package vfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/garyships/kudzu/internal/lru"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
)

// Node is one file or directory in the view.
type Node struct {
	Path string // "" for the root
	Mode uint32 // object.ModeDir, ModeFile, ModeExec or ModeSymlink
	ID   object.ID
	Size int64 // content length for files and symlinks
}

func (n Node) IsDir() bool { return n.Mode == object.ModeDir }

// FS serves one revision. It is safe for concurrent use.
type FS struct {
	Changeset object.ID
	Tree      object.ID
	Time      int64 // the changeset's time (unix seconds), or when it was opened

	remote *proto.Remote
	cache  store.Objects
	sparse sparse.Spec

	trees  *lru.Cache[object.ID, *object.Tree]
	sizes  *lru.Cache[object.ID, map[string]int64]
	mu     sync.Mutex
	flight map[object.ID]*fetch

	treesFetched atomic.Int64
	blobsFetched atomic.Int64
	bytesFetched atomic.Int64
}

type fetch struct {
	done chan struct{}
	data []byte
	err  error
}

// Open resolves rev on remote. cache keeps fetched objects (nil: memory
// only); sp limits the view to a sparse cone (nil: everything).
func Open(ctx context.Context, remote *proto.Remote, rev string, sp sparse.Spec, cache store.Objects) (*FS, error) {
	var res struct {
		Changeset object.ID `json:"changeset"`
		Tree      object.ID `json:"tree"`
	}
	if err := remote.GetJSON(ctx, "/api/resolve", url.Values{"rev": {rev}}, &res); err != nil {
		return nil, err
	}
	if cache == nil {
		cache = store.NewMem(256 << 20)
	}
	when := time.Now().Unix()
	var cs struct {
		Time int64 `json:"time"`
	}
	if remote.GetJSON(ctx, "/api/changesets/"+res.Changeset.String(), nil, &cs) == nil && cs.Time > 0 {
		when = cs.Time
	}
	return &FS{
		Changeset: res.Changeset,
		Tree:      res.Tree,
		Time:      when,
		remote:    remote,
		cache:     cache,
		sparse:    sp,
		trees:     lru.New[object.ID, *object.Tree](100_000),
		sizes:     lru.New[object.ID, map[string]int64](100_000),
		flight:    map[object.ID]*fetch{},
	}, nil
}

// Root is the top directory.
func (f *FS) Root() Node { return Node{Mode: object.ModeDir, ID: f.Tree} }

// Stats counts what the view has downloaded.
type Stats struct {
	TreesFetched int64 `json:"trees_fetched"`
	BlobsFetched int64 `json:"blobs_fetched"`
	BytesFetched int64 `json:"bytes_fetched"`
}

func (f *FS) Stats() Stats {
	return Stats{f.treesFetched.Load(), f.blobsFetched.Load(), f.bytesFetched.Load()}
}

// object returns an object's content from the local cache, or fetches it
// once (concurrent readers of the same object share one request).
func (f *FS) object(ctx context.Context, id object.ID, kind object.Kind) ([]byte, error) {
	if b, err := f.cache.Get(ctx, id); err == nil {
		return decode(id, b, kind)
	}
	f.mu.Lock()
	fl, ok := f.flight[id]
	if !ok {
		fl = &fetch{done: make(chan struct{})}
		f.flight[id] = fl
	}
	f.mu.Unlock()
	if ok {
		select {
		case <-fl.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if fl.err != nil {
			return nil, fl.err
		}
		return decode(id, fl.data, kind)
	}
	// Other readers may be waiting on this fetch: don't let this caller's
	// cancellation fail theirs.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	fl.data, fl.err = f.download(dctx, id, kind)
	cancel()
	close(fl.done)
	f.mu.Lock()
	delete(f.flight, id)
	f.mu.Unlock()
	if fl.err != nil {
		return nil, fl.err
	}
	return decode(id, fl.data, kind)
}

func (f *FS) download(ctx context.Context, id object.ID, kind object.Kind) ([]byte, error) {
	var stored []byte
	missing, err := f.remote.Fetch(ctx, []object.ID{id}, func(got object.ID, b []byte) error {
		if _, _, err := object.Verify(got, b); err != nil {
			return err
		}
		stored = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 || stored == nil {
		return nil, fmt.Errorf("%s %s: %w", kind, id.Short(), store.ErrNotFound)
	}
	if kind == object.KindTree {
		f.treesFetched.Add(1)
	} else {
		f.blobsFetched.Add(1)
	}
	f.bytesFetched.Add(int64(len(stored)))
	f.cache.Put(ctx, id, stored)
	return stored, nil
}

func decode(id object.ID, stored []byte, want object.Kind) ([]byte, error) {
	kind, content, err := object.Decode(stored)
	if err != nil {
		return nil, err
	}
	if kind != want {
		return nil, fmt.Errorf("object %s is a %s, not a %s", id.Short(), kind, want)
	}
	return content, nil
}

func (f *FS) tree(ctx context.Context, id object.ID) (*object.Tree, error) {
	if t, ok := f.trees.Get(id); ok {
		return t, nil
	}
	b, err := f.object(ctx, id, object.KindTree)
	if err != nil {
		return nil, err
	}
	t, err := object.DecodeTree(b)
	if err != nil {
		return nil, err
	}
	f.trees.Add(id, t)
	return t, nil
}

// sizesOf asks the server for the sizes of a directory's files, so their
// attributes are exact before (and without) fetching their contents.
func (f *FS) sizesOf(ctx context.Context, tree object.ID) (map[string]int64, error) {
	if m, ok := f.sizes.Get(tree); ok {
		return m, nil
	}
	m := map[string]int64{}
	if err := f.remote.GetJSON(ctx, "/api/sizes", url.Values{"tree": {tree.String()}}, &m); err != nil {
		return nil, err
	}
	f.sizes.Add(tree, m)
	return m, nil
}

// ReadDir lists a directory: one tree object and one size request the first
// time, nothing afterwards. Entries outside the sparse cone, submodules and
// reserved names (.git, .kudzu) are left out.
func (f *FS) ReadDir(ctx context.Context, dir Node) ([]Node, error) {
	if !dir.IsDir() {
		return nil, fmt.Errorf("%s: %w", dir.Path, errNotDir)
	}
	t, err := f.tree(ctx, dir.ID)
	if err != nil {
		return nil, err
	}
	var sizes map[string]int64
	out := make([]Node, 0, len(t.Entries))
	for _, e := range t.Entries {
		n, ok := f.node(dir.Path, e)
		if !ok {
			continue
		}
		if !n.IsDir() {
			if sizes == nil {
				if sizes, err = f.sizesOf(ctx, dir.ID); err != nil {
					return nil, err
				}
			}
			n.Size = sizes[e.Name]
		}
		out = append(out, n)
	}
	return out, nil
}

var errNotDir = errors.New("not a directory")

func (f *FS) node(parent string, e object.Entry) (Node, bool) {
	p := object.JoinPath(parent, e.Name)
	switch {
	case object.ReservedName(e.Name):
		// No server creates these, but a hostile one could send them: a
		// .git/config in a mount could make git run commands.
		return Node{}, false
	case e.IsDir():
		if !f.sparse.WantDir(p) {
			return Node{}, false
		}
	case e.Mode == object.ModeGitlink || !f.sparse.WantFile(p):
		return Node{}, false
	}
	return Node{Path: p, Mode: e.Mode, ID: e.ID}, true
}

// Lookup finds name in dir.
func (f *FS) Lookup(ctx context.Context, dir Node, name string) (Node, error) {
	entries, err := f.ReadDir(ctx, dir)
	if err != nil {
		return Node{}, err
	}
	for _, n := range entries {
		if n.Path == object.JoinPath(dir.Path, name) {
			return n, nil
		}
	}
	return Node{}, fs.ErrNotExist
}

// Stat finds a slash-separated path from the root.
func (f *FS) Stat(ctx context.Context, path string) (Node, error) {
	n := f.Root()
	if path == "" {
		return n, nil
	}
	for _, name := range splitPath(path) {
		next, err := f.Lookup(ctx, n, name)
		if err != nil {
			return Node{}, err
		}
		n = next
	}
	return n, nil
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// ReadFile returns a file's content (or a symlink's target), fetching the
// blob on first use.
func (f *FS) ReadFile(ctx context.Context, n Node) ([]byte, error) {
	if n.IsDir() {
		return nil, fmt.Errorf("%s: is a directory", n.Path)
	}
	return f.object(ctx, n.ID, object.KindBlob)
}
