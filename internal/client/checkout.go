package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
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
			if err := checkDir(runtime.GOOS, d.path, t.Entries, sp); err != nil {
				return nil, err
			}
			for _, e := range t.Entries {
				p := object.JoinPath(d.path, e.Name)
				if !materialized(e, p, sp) {
					continue
				}
				if e.IsDir() {
					next = append(next, dirRef{p, e.ID})
				} else {
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
			if err := checkDir(runtime.GOOS, pair.path, nt.Entries, sp); err != nil {
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
//
// Without force, apply refuses before changing anything if an untracked file
// is in the way, and it never removes what the index does not track. Nothing
// is written through a symlink: directories are created one component at a
// time, each write first checks the directories above it, and symlinks are
// created last, one at a time, so no link this checkout makes exists yet
// while files are written.
func (w *WC) apply(ctx context.Context, writes map[string]object.Entry, deletes []string, force bool) error {
	if !force {
		if err := w.checkUntracked(writes, deletes); err != nil {
			return err
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(deletes)))
	for _, p := range deletes {
		// Below something that is no longer a real directory, p is not in
		// the working copy, and removing it could follow a symlink out.
		if w.checkParents(p) == nil {
			if err := remove(w.abs(p), force); err != nil {
				return err
			}
			w.pruneEmptyDirs(filepath.Dir(w.abs(p)))
		}
		delete(w.Index, p)
	}

	type target struct {
		path    string
		mode    uint32
		tracked bool
	}
	byBlob := map[object.ID][]target{}
	dirs := map[string]bool{}
	for p, e := range writes {
		prev, tracked := w.Index[p]
		if tracked && prev.Blob == e.ID && prev.Mode == e.Mode {
			if info, err := os.Lstat(w.abs(p)); err == nil && info.Size() == prev.Size && info.ModTime().UnixNano() == prev.MtimeNs {
				continue
			}
		}
		byBlob[e.ID] = append(byBlob[e.ID], target{p, e.Mode, tracked})
		if d := path.Dir(p); d != "." {
			dirs[d] = true
		}
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
		if err := w.mkdirs(d, force); err != nil {
			return err
		}
	}

	type link struct {
		target
		id      object.ID
		content []byte
	}
	var links []link

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
					if t.mode == object.ModeSymlink {
						mu.Lock()
						links = append(links, link{t, it.id, content})
						mu.Unlock()
						continue
					}
					ie, err := w.writeFile(t.path, t.mode, it.id, content, t.tracked, force)
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
		fmt.Fprintf(w.Out, "fetching %d %s\n", len(remote), pick(len(remote) == 1, "object", "objects"))
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
	if fetchErr == nil && firstErr == nil {
		sort.Slice(links, func(i, j int) bool { return links[i].path < links[j].path })
		for _, l := range links {
			ie, err := w.writeFile(l.path, l.mode, l.id, l.content, l.tracked, force)
			if err != nil {
				firstErr = fmt.Errorf("%s: %w", l.path, err)
				break
			}
			written[l.path] = ie
		}
	}
	for p, ie := range written {
		w.Index[p] = ie
	}
	if fetchErr != nil {
		return fetchErr
	}
	return firstErr
}

// UntrackedError lists untracked files that a checkout would have
// overwritten or removed.
type UntrackedError struct {
	Paths []string
}

func (e *UntrackedError) Error() string {
	ps, more := e.Paths, ""
	if len(ps) > 10 {
		ps, more = ps[:10], fmt.Sprintf(" and %d more", len(ps)-10)
	}
	return "untracked files would be overwritten: " + strings.Join(ps, ", ") + more + " (move or remove them first)"
}

// checkUntracked finds what writing writes would overwrite that the index
// does not track and that deletes does not remove: a file at a path to
// write, files inside a directory that becomes a file, and a file or
// symlink where a directory is needed. Ignored files count too.
func (w *WC) checkUntracked(writes map[string]object.Entry, deletes []string) error {
	deleted := make(map[string]bool, len(deletes))
	for _, p := range deletes {
		deleted[p] = true
	}
	var byFold map[string][]fs.FileInfo
	expendable := func(p string, info fs.FileInfo) bool {
		if deleted[p] {
			return true
		}
		// On a case-insensitive filesystem a deleted file can turn up under
		// another spelling, as when README becomes readme.
		if byFold == nil {
			byFold = map[string][]fs.FileInfo{}
			for _, d := range deletes {
				if di, err := os.Lstat(w.abs(d)); err == nil {
					byFold[foldCase(d)] = append(byFold[foldCase(d)], di)
				}
			}
		}
		for _, di := range byFold[foldCase(p)] {
			if os.SameFile(info, di) {
				return true
			}
		}
		return false
	}
	var inWay []string
	isDir := map[string]bool{} // directories looked at, and whether one is there
	for p := range writes {
		if _, ok := w.Index[p]; ok {
			continue
		}
		absent := false // a directory above p is missing: nothing is at p
		for i := 0; i < len(p) && !absent; i++ {
			if p[i] != '/' {
				continue
			}
			d := p[:i]
			ok, seen := isDir[d]
			if !seen {
				info, err := os.Lstat(w.abs(d))
				switch {
				case err == nil && info.IsDir():
					ok = true
				case err == nil && !expendable(d, info):
					inWay = append(inWay, d)
				case err != nil && !errors.Is(err, fs.ErrNotExist):
					return err
				}
				isDir[d] = ok
			}
			absent = !ok
		}
		if absent {
			continue
		}
		info, err := os.Lstat(w.abs(p))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if !info.IsDir() {
			if !expendable(p, info) {
				inWay = append(inWay, p)
			}
			continue
		}
		err = filepath.WalkDir(w.abs(p), func(full string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(w.Root, full)
			if rel = filepath.ToSlash(rel); !expendable(rel, info) {
				inWay = append(inWay, rel)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(inWay) > 0 {
		sort.Strings(inWay)
		return &UntrackedError{Paths: inWay}
	}
	return nil
}

// checkParents makes sure every directory above p is a real directory, so a
// write or removal at p cannot follow a symlink out of the working copy (git
// refuses such a path as "beyond a symbolic link").
func (w *WC) checkParents(p string) error {
	for i := 0; i < len(p); i++ {
		if p[i] != '/' {
			continue
		}
		info, err := os.Lstat(w.abs(p[:i]))
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link, not a directory", p[:i])
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", p[:i])
		}
	}
	return nil
}

// mkdirs creates directory dir and its parents one component at a time. A
// file or symlink in the way is never followed: it is an error, or removed
// with force.
func (w *WC) mkdirs(dir string, force bool) error {
	for i := 1; i <= len(dir); i++ {
		if i < len(dir) && dir[i] != '/' {
			continue
		}
		full := w.abs(dir[:i])
		info, err := os.Lstat(full)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil && !force:
			return fmt.Errorf("cannot create directory %s: %s is in the way", dir, dir[:i])
		case err == nil:
			if err := os.Remove(full); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := os.Mkdir(full, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// remove deletes the file at a tracked path. Only force also removes a
// directory that has taken its place, and whatever is in it.
func remove(full string, force bool) error {
	var err error
	if force {
		err = os.RemoveAll(full)
	} else {
		err = os.Remove(full)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// writeFile writes one working-copy file after checking the directories
// above it. Whatever is at p is removed first, so a symlink or a stale mode
// never leaks into the new file; without force that must be a file the index
// tracks, or a directory with no files in it.
func (w *WC) writeFile(p string, mode uint32, id object.ID, content []byte, tracked, force bool) (IndexEntry, error) {
	if err := w.checkParents(p); err != nil {
		return IndexEntry{}, err
	}
	full := w.abs(p)
	if info, err := os.Lstat(full); err == nil {
		switch {
		case force:
			err = os.RemoveAll(full)
		case info.IsDir():
			err = removeEmptyDirs(full)
		case tracked:
			err = os.Remove(full)
		default:
			err = errors.New("an untracked file is in the way")
		}
		if err != nil {
			return IndexEntry{}, err
		}
	}
	var err error
	if mode == object.ModeSymlink {
		err = os.Symlink(string(content), full)
	} else {
		perm := os.FileMode(0o644)
		if mode == object.ModeExec {
			perm = 0o755
		}
		err = createFile(full, content, perm)
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

// createFile writes a new file. O_EXCL refuses to follow a symlink at full.
func createFile(full string, content []byte, perm os.FileMode) error {
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil && perm&0o111 != 0 {
		err = f.Chmod(perm) // the umask may have dropped exec bits
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeEmptyDirs removes dir if nothing but empty directories is inside.
func removeEmptyDirs(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			removeEmptyDirs(filepath.Join(dir, e.Name())) // what is left makes the Remove below fail
		}
	}
	return os.Remove(dir)
}

func (w *WC) pruneEmptyDirs(dir string) {
	for dir != w.Root && strings.HasPrefix(dir, w.Root) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
