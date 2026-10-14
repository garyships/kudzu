package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
)

// GCOptions controls garbage collection.
type GCOptions struct {
	// Keep is the grace period: changesets appended and objects written or
	// touched within it are kept even if no ref reaches them, along with
	// everything they need. It is what makes gc safe next to a live server,
	// which touches objects it hands out or reuses.
	Keep time.Duration
	// DryRun reports what would be removed without removing anything.
	DryRun bool
	// Now overrides the current time (tests).
	Now time.Time
}

// GCResult reports what garbage collection kept and removed.
type GCResult struct {
	Changesets        int   `json:"changesets"`
	RemovedChangesets int   `json:"removed_changesets"`
	Objects           int   `json:"objects"`
	RemovedObjects    int   `json:"removed_objects"`
	RemovedBytes      int64 `json:"removed_bytes"`
	RemovedDerived    int   `json:"removed_derived"`
	RemovedMappings   int   `json:"removed_mappings"`
	DryRun            bool  `json:"dry_run,omitempty"`
}

type gcChangeset struct {
	parents []object.ID
	created int64
}

// GC removes changesets and objects that nothing needs any more: changesets
// that no ref reaches, created before the grace period, together with their
// derived data, git mappings, and every tree, unode and blob that only they
// used. Objects newer than the grace period always stay.
//
// The changeset log keeps its positions: removed entries leave a gap in seq,
// which /api/wal readers skip naturally, so a reader's cursor stays valid.
func (r *Repo) GC(ctx context.Context, opts GCOptions) (*GCResult, error) {
	sw, ok := r.Objs.(store.Sweeper)
	if !ok {
		return nil, fmt.Errorf("object store %T cannot be garbage collected", r.Objs)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.Keep)
	res := &GCResult{DryRun: opts.DryRun}

	// Read the whole log. Changesets appended after this point are newer
	// than the cutoff, so they are never candidates.
	all := map[object.ID]*gcChangeset{}
	for since := int64(0); ; {
		entries, err := r.Meta.WAL(ctx, since, 10000)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			c, err := object.DecodeChangeset(e.Raw)
			if err != nil {
				return nil, fmt.Errorf("changeset %s: %w", e.ID.Short(), err)
			}
			all[e.ID] = &gcChangeset{parents: c.Parents, created: e.Created}
			since = e.Seq
		}
	}
	res.Changesets = len(all)

	// Mark: changesets reachable from refs or from anything inside the grace
	// period, then every object those changesets use.
	live := map[object.ID]bool{}
	var roots []object.ID
	for id, c := range all {
		if c.created >= cutoff.Unix() {
			roots = append(roots, id)
		}
	}
	m := &marker{r: r, objs: map[object.ID]bool{}}
	markRefs := func() error {
		refs, err := r.Meta.ListRefs(ctx)
		if err != nil {
			return err
		}
		for _, id := range refs {
			roots = append(roots, id)
		}
		for len(roots) > 0 {
			id := roots[len(roots)-1]
			roots = roots[:len(roots)-1]
			c, ok := all[id]
			if !ok || live[id] {
				continue // already marked, or outside the log (shallow import)
			}
			live[id] = true
			if err := m.changeset(ctx, id); err != nil {
				return err
			}
			roots = append(roots, c.parents...)
		}
		return nil
	}
	if err := markRefs(); err != nil {
		return nil, err
	}
	// Refs may have moved while we marked; pick up anything they now reach.
	if err := markRefs(); err != nil {
		return nil, err
	}

	var deadCS []object.ID
	dead := map[string]bool{}
	for id, c := range all {
		if !live[id] && c.created < cutoff.Unix() {
			deadCS = append(deadCS, id)
			dead[id.String()] = true
		}
	}
	var deadObjs []object.ID
	err := sw.Walk(ctx, func(id object.ID, size int64, mtime time.Time) error {
		res.Objects++
		if !m.objs[id] && mtime.Before(cutoff) {
			deadObjs = append(deadObjs, id)
			if opts.DryRun {
				res.RemovedObjects++
				res.RemovedBytes += size
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if opts.DryRun {
		res.RemovedChangesets = len(deadCS)
		return res, nil
	}

	// Sweep: changesets first, so that nothing can be built on them while
	// their objects go away. A changeset re-appended since the scan has a new
	// created time and is skipped.
	if res.RemovedChangesets, err = r.Meta.DeleteChangesets(ctx, deadCS, cutoff.Unix()); err != nil {
		return nil, err
	}
	for _, id := range deadCS {
		r.changesets.Remove(id)
	}
	type mapping struct{ ns, key string }
	var deadMappings []mapping
	err = r.Meta.Mappings(ctx, "", func(ns, key, value string) error {
		if dead[key] || dead[value] {
			deadMappings = append(deadMappings, mapping{ns, key})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, dm := range deadMappings {
		if err := r.Meta.DeleteMapping(ctx, dm.ns, dm.key); err != nil {
			return nil, err
		}
		res.RemovedMappings++
	}
	var removedObjs []object.ID
	for _, id := range deadObjs {
		removed, size, err := sw.DeleteIfOlder(ctx, id, cutoff)
		if err != nil {
			return nil, err
		}
		if removed {
			res.RemovedObjects++
			res.RemovedBytes += size
			r.trees.Remove(id)
			r.unodes.Remove(id)
			removedObjs = append(removedObjs, id)
		}
	}
	// Derived data is keyed by changeset (tree, gen, unode) or by file unode
	// (blame); removed objects that are not unodes simply match nothing.
	for _, d := range Derivers {
		keys := deadCS
		if d == BlameDeriver {
			keys = removedObjs
		}
		n, err := r.Meta.DeleteDerived(ctx, d.Name, keys)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			r.derived.Remove(derivedKey{d.Name, k})
		}
		res.RemovedDerived += n
	}
	if fs, ok := r.Objs.(interface{ CleanTmp(time.Time) (int, error) }); ok {
		fs.CleanTmp(cutoff)
	}
	return res, nil
}

// marker records every object a live changeset needs.
type marker struct {
	r    *Repo
	objs map[object.ID]bool
}

func (m *marker) changeset(ctx context.Context, id object.ID) error {
	c, err := m.r.Changeset(ctx, id)
	if err != nil {
		return err
	}
	for _, ch := range c.Changes {
		if !ch.Deleted() {
			m.objs[ch.Blob] = true
		}
	}
	tree, err := m.r.TreeOf(ctx, id)
	if err != nil {
		return err
	}
	if err := m.tree(ctx, tree); err != nil {
		return err
	}
	// Unodes only exist once derived; don't derive them just to mark them.
	if v, ok, err := m.r.getDerived(ctx, UnodeDeriver.Name, id); err != nil {
		return err
	} else if ok {
		return m.unode(ctx, idValue(v))
	}
	return nil
}

// tree marks a tree and everything below it, skipping subtrees it has
// already seen, so marking all of history costs about the number of
// distinct trees.
func (m *marker) tree(ctx context.Context, id object.ID) error {
	if id.IsZero() || m.objs[id] {
		return nil
	}
	m.objs[id] = true
	t, err := m.r.ReadTree(ctx, id)
	if err != nil {
		return err
	}
	for _, e := range t.Entries {
		if e.IsDir() {
			if err := m.tree(ctx, e.ID); err != nil {
				return err
			}
		} else {
			m.objs[e.ID] = true // blobs, including gitlink placeholders
		}
	}
	return nil
}

// unode marks a unode, its children and its previous versions.
func (m *marker) unode(ctx context.Context, id object.ID) error {
	stack := []object.ID{id}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id.IsZero() || m.objs[id] {
			continue
		}
		m.objs[id] = true
		u, err := m.r.ReadUnode(ctx, id)
		if err != nil {
			return err
		}
		stack = append(stack, u.Parents...)
		if u.Dir {
			for _, e := range u.Entries {
				stack = append(stack, e.ID)
			}
		} else {
			m.objs[u.Content] = true
		}
	}
	return nil
}
