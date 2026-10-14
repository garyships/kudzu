package server

import (
	"context"
	"time"

	"github.com/garyships/kudzu/internal/auth"
	"github.com/garyships/kudzu/internal/object"
)

// Objects are fetched by ID from any tier, so read access to objects comes
// down to which IDs a caller learns. A caller with restricted read access
// never sees the ID of a tree that holds something it may not read: wherever
// the API returns a tree, it returns a filtered copy with those entries
// removed. Filtered trees are ordinary tree objects (clients verify them
// like any other), so checkouts, updates and snapshots work unchanged, and
// subtrees the caller may read in full keep their real IDs.

type viewKey struct {
	principal string
	dir       string
	tree      object.ID
}

type viewEntry struct {
	id   object.ID
	made time.Time
}

// viewTTL bounds how long a filtered tree is reused before it is rebuilt.
// Rebuilding rewrites (touches) the filtered objects, which no changeset
// references, so garbage collection's grace period keeps them while they are
// handed out.
const viewTTL = 5 * time.Minute

// treeFor returns the root tree of changeset id as the caller may see it.
func (s *Server) treeFor(ctx context.Context, id object.ID) (object.ID, error) {
	tree, err := s.Repo.TreeOf(ctx, id)
	if err != nil {
		return object.Zero, err
	}
	return s.viewTree(ctx, tree)
}

// viewTree filters a root tree for the caller.
func (s *Server) viewTree(ctx context.Context, tree object.ID) (object.ID, error) {
	p := auth.FromContext(ctx)
	if p.ReadsAll("") {
		return tree, nil
	}
	return s.viewDir(ctx, p, tree, "")
}

func (s *Server) viewDir(ctx context.Context, p *auth.Principal, id object.ID, dir string) (object.ID, error) {
	if p.ReadsAll(dir) {
		return id, nil
	}
	key := viewKey{p.Name, dir, id}
	if v, ok := s.views.Get(key); ok && time.Since(v.made) < viewTTL {
		return v.id, nil
	}
	t, err := s.Repo.ReadTree(ctx, id)
	if err != nil {
		return object.Zero, err
	}
	out := make([]object.Entry, 0, len(t.Entries))
	changed := false
	for _, e := range t.Entries {
		path := object.JoinPath(dir, e.Name)
		if e.IsDir() {
			if !p.CanSee(path) {
				changed = true
				continue
			}
			sub, err := s.viewDir(ctx, p, e.ID, path)
			if err != nil {
				return object.Zero, err
			}
			if sub == object.EmptyTreeID {
				changed = true // nothing readable below: drop it, as trees never hold empty directories
				continue
			}
			if sub != e.ID {
				changed = true
				e.ID = sub
			}
		} else if !p.CanRead(path) {
			changed = true
			continue
		}
		out = append(out, e)
	}
	result := id
	if changed {
		if result, err = s.Repo.WriteTree(ctx, &object.Tree{Entries: out}); err != nil {
			return object.Zero, err
		}
	}
	s.views.Add(key, viewEntry{result, time.Now()})
	return result, nil
}

// visibleEntries filters a directory listing at changeset cs for a caller
// that may not read all of dir.
func (s *Server) visibleEntries(ctx context.Context, cs object.ID, dir string, entries []object.Entry) ([]object.Entry, error) {
	p := auth.FromContext(ctx)
	if dir != "" {
		tree, err := s.Repo.TreeOf(ctx, cs)
		if err != nil {
			return nil, err
		}
		if e, ok, err := s.Repo.LookupPath(ctx, tree, dir); err != nil {
			return nil, err
		} else if ok && !e.IsDir() {
			return entries, p.CheckRead(dir) // listing a single file
		}
	}
	out := make([]object.Entry, 0, len(entries))
	for _, e := range entries {
		path := object.JoinPath(dir, e.Name)
		if e.IsDir() {
			if !p.CanSee(path) {
				continue
			}
			sub, err := s.viewDir(ctx, p, e.ID, path)
			if err != nil {
				return nil, err
			}
			if sub == object.EmptyTreeID {
				continue
			}
			e.ID = sub
		} else if !p.CanRead(path) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
