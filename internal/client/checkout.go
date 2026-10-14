package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
)

// ensure fetches whichever of ids are not in the local cache. This is the
// whole negotiation: the client names the objects it wants, never the ones
// it has.
func (w *WC) ensure(ctx context.Context, ids []object.ID) error {
	var want []object.ID
	seen := map[object.ID]bool{}
	for _, id := range ids {
		if id.IsZero() || seen[id] {
			continue
		}
		seen[id] = true
		if ok, err := w.Objs.Has(ctx, id); err != nil {
			return err
		} else if !ok {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}
	missing, err := w.Remote.Fetch(ctx, want, func(id object.ID, b []byte) error {
		if _, _, err := object.Verify(id, b); err != nil {
			return err
		}
		return w.Objs.Put(ctx, id, b)
	})
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("remote is missing %d objects (e.g. %s)", len(missing), missing[0].Short())
	}
	return nil
}

func (w *WC) readObject(ctx context.Context, id object.ID, kind object.Kind) ([]byte, error) {
	b, err := w.Objs.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		if err := w.ensure(ctx, []object.ID{id}); err != nil {
			return nil, err
		}
		b, err = w.Objs.Get(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	k, content, err := object.Decode(b)
	if err != nil {
		return nil, err
	}
	if k != kind {
		return nil, fmt.Errorf("object %s is a %s, not a %s", id.Short(), k, kind)
	}
	return content, nil
}

func (w *WC) readTree(ctx context.Context, id object.ID) (*object.Tree, error) {
	if id.IsZero() {
		return &object.Tree{}, nil
	}
	b, err := w.readObject(ctx, id, object.KindTree)
	if err != nil {
		return nil, err
	}
	return object.DecodeTree(b)
}

func (w *WC) readBlob(ctx context.Context, id object.ID) ([]byte, error) {
	return w.readObject(ctx, id, object.KindBlob)
}

type dirRef struct {
	path string
	id   object.ID
}

// collectFiles walks the tree one level at a time, fetching each level's
// missing trees in one batch, and returns every file the spec includes.
// Directories outside the spec are never requested.
func (w *WC) collectFiles(ctx context.Context, root object.ID, sp sparse.Spec) (map[string]object.Entry, error) {
	files := map[string]object.Entry{}
	level := []dirRef{{"", root}}
	for len(level) > 0 {
		ids := make([]object.ID, len(level))
		for i, d := range level {
			ids[i] = d.id
		}
		if err := w.ensure(ctx, ids); err != nil {
			return nil, err
		}
		var next []dirRef
		for _, d := range level {
			t, err := w.readTree(ctx, d.id)
			if err != nil {
				return nil, err
			}
			for _, e := range t.Entries {
				p := object.JoinPath(d.path, e.Name)
				if p == metaDir {
					return nil, fmt.Errorf("remote tree contains reserved path %s", metaDir)
				}
				if e.IsDir() {
					if sp.WantDir(p) {
						next = append(next, dirRef{p, e.ID})
					}
				} else if e.Mode != object.ModeGitlink && sp.WantFile(p) {
					files[p] = e
				}
			}
		}
		level = next
	}
	return files, nil
}

type treePair struct {
	path     string
	old, new object.ID
}

// diffTrees finds what changes between two trees within the spec. Equal
// subtrees are skipped without being fetched, so moving a checkout between
// nearby changesets costs about the size of the difference.
func (w *WC) diffTrees(ctx context.Context, oldRoot, newRoot object.ID, sp sparse.Spec) (writes map[string]object.Entry, deletes []string, err error) {
	writes = map[string]object.Entry{}
	level := []treePair{{"", oldRoot, newRoot}}
	for len(level) > 0 {
		var ids []object.ID
		for _, p := range level {
			ids = append(ids, p.old, p.new)
		}
		if err := w.ensure(ctx, ids); err != nil {
			return nil, nil, err
		}
		var next []treePair
		for _, pair := range level {
			if pair.old == pair.new {
				continue
			}
			ot, err := w.readTree(ctx, pair.old)
			if err != nil {
				return nil, nil, err
			}
			nt, err := w.readTree(ctx, pair.new)
			if err != nil {
				return nil, nil, err
			}
			oe, ne := ot.Entries, nt.Entries
			for i, j := 0, 0; i < len(oe) || j < len(ne); {
				var o, n *object.Entry
				switch {
				case j >= len(ne) || (i < len(oe) && oe[i].Name < ne[j].Name):
					o = &oe[i]
					i++
				case i >= len(oe) || ne[j].Name < oe[i].Name:
					n = &ne[j]
					j++
				default:
					o, n = &oe[i], &ne[j]
					i++
					j++
				}
				name := ""
				if o != nil {
					name = o.Name
				} else {
					name = n.Name
				}
				p := object.JoinPath(pair.path, name)
				if p == metaDir {
					return nil, nil, fmt.Errorf("remote tree contains reserved path %s", metaDir)
				}
				if o != nil && n != nil && *o == *n {
					continue
				}
				var oldDir, newDir object.ID
				if o != nil && o.IsDir() {
					oldDir = o.ID
				}
				if n != nil && n.IsDir() {
					newDir = n.ID
				}
				if !oldDir.IsZero() || !newDir.IsZero() {
					if sp.WantDir(p) {
						next = append(next, treePair{p, oldDir, newDir})
					}
				}
				if o != nil && !o.IsDir() && sp.WantFile(p) {
					deletes = append(deletes, p)
				}
				if n != nil && !n.IsDir() && n.Mode != object.ModeGitlink && sp.WantFile(p) {
					writes[p] = *n
				}
			}
		}
		level = next
	}
	// A path both deleted and written is a modification.
	kept := deletes[:0]
	for _, p := range deletes {
		if _, ok := writes[p]; !ok {
			kept = append(kept, p)
		}
	}
	return writes, kept, nil
}

// apply removes deletes, then materializes writes. Blobs are written to the
// working copy straight from the fetch stream on a worker pool; they are only
// kept in the local object cache when no node daemon is caching them already.
func (w *WC) apply(ctx context.Context, writes map[string]object.Entry, deletes []string) error {
	sort.Sort(sort.Reverse(sort.StringSlice(deletes)))
	for _, p := range deletes {
		if err := os.RemoveAll(w.abs(p)); err != nil {
			return err
		}
		delete(w.Index, p)
		w.pruneEmptyDirs(filepath.Dir(w.abs(p)))
	}

	type target struct {
		path    string
		mode    uint32
		existed bool
	}
	byBlob := map[object.ID][]target{}
	dirs := map[string]bool{}
	for p, e := range writes {
		prev, existed := w.Index[p]
		if existed && prev.Blob == e.ID && prev.Mode == e.Mode {
			if info, err := os.Lstat(w.abs(p)); err == nil && info.Size() == prev.Size && info.ModTime().UnixNano() == prev.MtimeNs {
				continue
			}
		}
		byBlob[e.ID] = append(byBlob[e.ID], target{p, e.Mode, existed})
		dirs[filepath.Dir(w.abs(p))] = true
	}
	if len(byBlob) == 0 {
		return nil
	}
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	for _, d := range dirList {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	type item struct {
		id     object.ID
		stored []byte
		remote bool
	}
	items := make(chan item, 256)
	var mu sync.Mutex
	var firstErr error
	written := map[string]IndexEntry{}
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range items {
				kind, content, err := object.Verify(it.id, it.stored)
				if err == nil && kind != object.KindBlob {
					err = fmt.Errorf("object %s is a %s, not a blob", it.id.Short(), kind)
				}
				if err != nil {
					fail(err)
					continue
				}
				if it.remote && w.State.CacheBlobs {
					if err := w.Objs.Put(ctx, it.id, it.stored); err != nil {
						fail(err)
					}
				}
				for _, t := range byBlob[it.id] {
					ie, err := w.writeFile(t.path, t.mode, it.id, content, t.existed)
					if err != nil {
						fail(fmt.Errorf("%s: %w", t.path, err))
						continue
					}
					mu.Lock()
					written[t.path] = ie
					mu.Unlock()
				}
			}
		}()
	}

	var remote []object.ID
	for id := range byBlob {
		if stored, err := w.Objs.Get(ctx, id); err == nil {
			items <- item{id, stored, false}
		} else {
			remote = append(remote, id)
		}
	}
	var fetchErr error
	if len(remote) > 0 {
		fmt.Fprintf(w.Out, "fetching %d objects\n", len(remote))
		var missing []object.ID
		missing, fetchErr = w.Remote.Fetch(ctx, remote, func(id object.ID, b []byte) error {
			items <- item{id, b, true}
			return nil
		})
		if fetchErr == nil && len(missing) > 0 {
			fetchErr = fmt.Errorf("remote is missing %d objects (e.g. %s)", len(missing), missing[0].Short())
		}
	}
	close(items)
	wg.Wait()
	for p, ie := range written {
		w.Index[p] = ie
	}
	if fetchErr != nil {
		return fetchErr
	}
	return firstErr
}

// writeFile writes one working-copy file. Files that existed are removed
// first, so a symlink or a stale mode never leaks into the new file.
func (w *WC) writeFile(p string, mode uint32, id object.ID, content []byte, existed bool) (IndexEntry, error) {
	full := w.abs(p)
	if existed {
		os.Remove(full)
	}
	var err error
	if mode == object.ModeSymlink {
		if err = os.Symlink(string(content), full); err != nil && !existed {
			os.RemoveAll(full)
			err = os.Symlink(string(content), full)
		}
	} else {
		perm := os.FileMode(0o644)
		if mode == object.ModeExec {
			perm = 0o755
		}
		if err = os.WriteFile(full, content, perm); err != nil {
			// Something else (e.g. a leftover directory) is in the way.
			os.RemoveAll(full)
			err = os.WriteFile(full, content, perm)
		}
		if err == nil && mode == object.ModeExec {
			err = os.Chmod(full, perm) // the umask may have dropped exec bits
		}
	}
	if err != nil {
		return IndexEntry{}, err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return IndexEntry{}, err
	}
	return IndexEntry{Mode: mode, Blob: id, Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}, nil
}

func (w *WC) pruneEmptyDirs(dir string) {
	for dir != w.Root && strings.HasPrefix(dir, w.Root) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
