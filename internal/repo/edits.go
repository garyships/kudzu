package repo

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/sparse"
)

// PathError reports a change that does not fit the directory structure it is
// applied to (deleting a missing file, a file where a directory is needed...).
type PathError struct {
	Path   string
	Reason string
}

func (e *PathError) Error() string { return e.Path + ": " + e.Reason }

// editNode is the set of changes below one directory, so applying a
// changeset rewrites only the directories on the path to each changed file.
type editNode struct {
	change   *object.Change
	children map[string]*editNode
}

func buildEdits(changes []object.Change) *editNode {
	root := &editNode{}
	for i := range changes {
		n := root
		for _, c := range strings.Split(changes[i].Path, "/") {
			if n.children == nil {
				n.children = map[string]*editNode{}
			}
			child, ok := n.children[c]
			if !ok {
				child = &editNode{}
				n.children[c] = child
			}
			n = child
		}
		n.change = &changes[i]
	}
	return root
}

// dirOps abstracts over plain trees and history-aware unodes so the same
// edit logic derives both.
type dirOps interface {
	readDir(ctx context.Context, id object.ID) ([]object.Entry, error)
	writeDir(ctx context.Context, entries []object.Entry, prev object.ID) (object.ID, error)
	writeFile(ctx context.Context, ch object.Change, prev object.Entry, hasPrev bool) (object.Entry, error)
}

// applyEdits applies e to directory dirID and returns the new directory and
// its entry count. Emptied non-root directories are pruned (zero ID).
func applyEdits(ctx context.Context, ops dirOps, dirID object.ID, e *editNode, path string) (object.ID, int, error) {
	entries, err := ops.readDir(ctx, dirID)
	if err != nil {
		return object.Zero, 0, err
	}
	m := make(map[string]object.Entry, len(entries)+len(e.children))
	for _, en := range entries {
		m[en.Name] = en
	}
	names := make([]string, 0, len(e.children))
	for name := range e.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := e.children[name]
		p := object.JoinPath(path, name)
		old, hasOld := m[name]
		oldIsFile := hasOld && !old.IsDir()
		if len(child.children) > 0 {
			var oldDir object.ID
			if hasOld && old.IsDir() {
				oldDir = old.ID
			} else if oldIsFile && (child.change == nil || !child.change.Deleted()) {
				return object.Zero, 0, &PathError{p, "is a file, not a directory"}
			}
			newDir, n, err := applyEdits(ctx, ops, oldDir, child, p)
			if err != nil {
				return object.Zero, 0, err
			}
			if n == 0 {
				delete(m, name)
			} else {
				m[name] = object.Entry{Name: name, Mode: object.ModeDir, ID: newDir}
			}
			if ch := child.change; ch != nil {
				switch {
				case ch.Deleted() && !oldIsFile:
					return object.Zero, 0, &PathError{p, "no such file to delete"}
				case !ch.Deleted() && n != 0:
					return object.Zero, 0, &PathError{p, "is a directory"}
				case !ch.Deleted():
					en, err := ops.writeFile(ctx, *ch, old, oldIsFile)
					if err != nil {
						return object.Zero, 0, err
					}
					en.Name = name
					m[name] = en
				}
			}
			continue
		}
		ch := child.change
		if ch.Deleted() {
			if !oldIsFile {
				return object.Zero, 0, &PathError{p, "no such file to delete"}
			}
			delete(m, name)
			continue
		}
		if hasOld && old.IsDir() {
			return object.Zero, 0, &PathError{p, "is a directory"}
		}
		en, err := ops.writeFile(ctx, *ch, old, oldIsFile)
		if err != nil {
			return object.Zero, 0, err
		}
		en.Name = name
		m[name] = en
	}
	out := make([]object.Entry, 0, len(m))
	for _, en := range m {
		out = append(out, en)
	}
	object.SortEntries(out)
	if len(out) == 0 && path != "" {
		return object.Zero, 0, nil
	}
	id, err := ops.writeDir(ctx, out, dirID)
	return id, len(out), err
}

type treeOps struct{ r *Repo }

func (o treeOps) readDir(ctx context.Context, id object.ID) ([]object.Entry, error) {
	t, err := o.r.ReadTree(ctx, id)
	if err != nil {
		return nil, err
	}
	return t.Entries, nil
}

func (o treeOps) writeDir(ctx context.Context, entries []object.Entry, _ object.ID) (object.ID, error) {
	return o.r.WriteTree(ctx, &object.Tree{Entries: entries})
}

func (o treeOps) writeFile(_ context.Context, ch object.Change, _ object.Entry, _ bool) (object.Entry, error) {
	return object.Entry{Mode: ch.Mode, ID: ch.Blob}, nil
}

type unodeOps struct {
	r    *Repo
	link object.ID
}

func (o unodeOps) readDir(ctx context.Context, id object.ID) ([]object.Entry, error) {
	if id.IsZero() {
		return nil, nil
	}
	u, err := o.r.ReadUnode(ctx, id)
	if err != nil {
		return nil, err
	}
	return u.Entries, nil
}

func (o unodeOps) writeDir(ctx context.Context, entries []object.Entry, prev object.ID) (object.ID, error) {
	u := &object.Unode{Dir: true, Linknode: o.link, Entries: entries}
	if !prev.IsZero() {
		u.Parents = []object.ID{prev}
	}
	return o.r.WriteUnode(ctx, u)
}

func (o unodeOps) writeFile(ctx context.Context, ch object.Change, prev object.Entry, hasPrev bool) (object.Entry, error) {
	u := &object.Unode{Linknode: o.link, Mode: ch.Mode, Content: ch.Blob}
	if hasPrev {
		u.Parents = []object.ID{prev.ID}
	}
	id, err := o.r.WriteUnode(ctx, u)
	return object.Entry{Mode: ch.Mode, ID: id}, err
}

// applyToTree returns the root tree produced by applying changes to base.
func (r *Repo) applyToTree(ctx context.Context, base object.ID, changes []object.Change) (object.ID, error) {
	id, _, err := applyEdits(ctx, treeOps{r}, base, buildEdits(changes), "")
	return id, err
}

// lookup finds path below root ("" is root itself, returned as a dir entry).
func lookup(ctx context.Context, ops dirOps, root object.ID, path string) (object.Entry, bool, error) {
	cur := object.Entry{Mode: object.ModeDir, ID: root}
	if path == "" {
		return cur, true, nil
	}
	for _, name := range strings.Split(path, "/") {
		if !cur.IsDir() {
			return object.Entry{}, false, nil
		}
		entries, err := ops.readDir(ctx, cur.ID)
		if err != nil {
			return object.Entry{}, false, err
		}
		i := sort.Search(len(entries), func(i int) bool { return entries[i].Name >= name })
		if i == len(entries) || entries[i].Name != name {
			return object.Entry{}, false, nil
		}
		cur = entries[i]
	}
	return cur, true, nil
}

func (r *Repo) LookupPath(ctx context.Context, tree object.ID, path string) (object.Entry, bool, error) {
	return lookup(ctx, treeOps{r}, tree, path)
}

// FileDiff describes one file that differs between two trees.
type FileDiff struct {
	Path string        `json:"path"`
	Old  *object.Entry `json:"old,omitempty"`
	New  *object.Entry `json:"new,omitempty"`
}

// DiffTrees reports every file that differs between trees a and b. Subtrees
// with equal IDs are skipped without being read, so the cost follows the size
// of the difference, not the size of the repository.
func (r *Repo) DiffTrees(ctx context.Context, a, b object.ID, prefix string, fn func(FileDiff) error) error {
	if a == b {
		return nil
	}
	ta, err := r.ReadTree(ctx, a)
	if err != nil {
		return err
	}
	tb, err := r.ReadTree(ctx, b)
	if err != nil {
		return err
	}
	ea, eb := ta.Entries, tb.Entries
	for i, j := 0, 0; i < len(ea) || j < len(eb); {
		var x, y *object.Entry
		switch {
		case j >= len(eb) || (i < len(ea) && ea[i].Name < eb[j].Name):
			x = &ea[i]
			i++
		case i >= len(ea) || eb[j].Name < ea[i].Name:
			y = &eb[j]
			j++
		default:
			x, y = &ea[i], &eb[j]
			i++
			j++
		}
		var name string
		if x != nil {
			name = x.Name
		} else {
			name = y.Name
		}
		p := object.JoinPath(prefix, name)
		if x != nil && y != nil && x.Mode == y.Mode && x.ID == y.ID {
			continue
		}
		if x != nil && y != nil && x.IsDir() && y.IsDir() {
			if err := r.DiffTrees(ctx, x.ID, y.ID, p, fn); err != nil {
				return err
			}
			continue
		}
		if x != nil && x.IsDir() {
			if err := r.WalkTree(ctx, x.ID, p, func(path string, e object.Entry) error {
				return fn(FileDiff{Path: path, Old: &e})
			}); err != nil {
				return err
			}
			x = nil
		}
		if y != nil && y.IsDir() {
			if err := r.WalkTree(ctx, y.ID, p, func(path string, e object.Entry) error {
				return fn(FileDiff{Path: path, New: &e})
			}); err != nil {
				return err
			}
			y = nil
		}
		if x != nil || y != nil {
			if err := fn(FileDiff{Path: p, Old: x, New: y}); err != nil {
				return err
			}
		}
	}
	return nil
}

// WalkTree calls fn for every file below tree id.
func (r *Repo) WalkTree(ctx context.Context, id object.ID, prefix string, fn func(path string, e object.Entry) error) error {
	t, err := r.ReadTree(ctx, id)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		p := object.JoinPath(prefix, e.Name)
		if e.IsDir() {
			err = r.WalkTree(ctx, e.ID, p, fn)
		} else {
			err = fn(p, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// WalkObjects calls fn for every tree and blob reachable from tree id,
// restricted to the given sparse directories (all if empty).
func (r *Repo) WalkObjects(ctx context.Context, id object.ID, sp sparse.Spec, fn func(id object.ID) error) error {
	seen := map[object.ID]bool{}
	var walk func(id object.ID, path string) error
	walk = func(id object.ID, path string) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		if err := fn(id); err != nil {
			return err
		}
		t, err := r.ReadTree(ctx, id)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := object.JoinPath(path, e.Name)
			if e.IsDir() {
				if sp.WantDir(p) {
					if err := walk(e.ID, p); err != nil {
						return err
					}
				}
				continue
			}
			if e.Mode == object.ModeGitlink || seen[e.ID] || !sp.WantFile(p) {
				continue
			}
			seen[e.ID] = true
			if err := fn(e.ID); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(id, "")
}

func asPathError(err error) (*PathError, bool) {
	var pe *PathError
	ok := errors.As(err, &pe)
	return pe, ok
}
