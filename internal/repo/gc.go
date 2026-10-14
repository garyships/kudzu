package repo

import (
	"context"
	"errors"
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

	// Read the whole log.
	all := map[object.ID]*gcChangeset{}
	lastSeq, err := r.scanLog(ctx, 0, all)
	if err != nil {
		return nil, err
	}
	res.Changesets = len(all)

	// Mark: changesets reachable from refs or from anything inside the grace
	// period, then every object those changesets use.
	live := map[object.ID]bool{}
	m := &marker{r: r, objs: map[object.ID]bool{}}
	mark := func(roots []object.ID) error {
		for len(roots) > 0 {
			id := roots[len(roots)-1]
			roots = roots[:len(roots)-1]
			if live[id] {
				continue
			}
			c, ok := all[id]
			if !ok {
				// Appended since the scan, or a shallow import's missing
				// parent: ask the log.
				raw, _, err := r.Meta.GetChangeset(ctx, id)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				cs, err := object.DecodeChangeset(raw)
				if err != nil {
					return err
				}
				c = &gcChangeset{parents: cs.Parents, created: now.Unix()}
				all[id] = c
			}
			live[id] = true
			if err := m.changeset(ctx, id); err != nil {
				return err
			}
			roots = append(roots, c.parents...)
		}
		return nil
	}
	markRefs := func() error {
		refs, err := r.Meta.ListRefs(ctx)
		if err != nil {
			return err
		}
		roots := make([]object.ID, 0, len(refs))
		for _, id := range refs {
			roots = append(roots, id)
		}
		return mark(roots)
	}
	var recent []object.ID
	for id, c := range all {
		if c.created >= cutoff.Unix() {
			recent = append(recent, id)
		}
	}
	if err := mark(recent); err != nil {
		return nil, err
	}
	if err := markRefs(); err != nil {
		return nil, err
	}

	type candidate struct {
		id   object.ID
		size int64
	}
	var candidates []candidate
	err = sw.Walk(ctx, func(id object.ID, size int64, mtime time.Time) error {
		res.Objects++
		if !m.objs[id] && mtime.Before(cutoff) {
			candidates = append(candidates, candidate{id, size})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Walking a large store takes a while. Mark again from whatever was
	// appended and wherever refs moved meanwhile, right before sweeping.
	appended := map[object.ID]*gcChangeset{}
	if _, err := r.scanLog(ctx, lastSeq, appended); err != nil {
		return nil, err
	}
	var newRoots []object.ID
	for id, c := range appended {
		all[id] = c
		newRoots = append(newRoots, id)
	}
	if err := mark(newRoots); err != nil {
		return nil, err
	}
	if err := markRefs(); err != nil {
		return nil, err
	}

	var deadCS []object.ID
	for id, c := range all {
		if !live[id] && c.created < cutoff.Unix() {
			deadCS = append(deadCS, id)
		}
	}
	if opts.DryRun {
		res.RemovedChangesets = len(deadCS)
		for _, c := range candidates {
			if !m.objs[c.id] {
				res.RemovedObjects++
				res.RemovedBytes += c.size
			}
		}
		return res, nil
	}

	// Sweep changesets first, so that nothing can be built on them while
	// their objects go away, and children before parents. Deletion is
	// conditional on the entry still being old: one that something has used
	// since (a new child or a ref move touches it) survives, and then so do
	// its ancestors and everything it needs.
	deleted := map[string]bool{}
	var deletedIDs []object.ID
	protected := map[object.ID]bool{}
	for _, id := range childrenFirst(deadCS, all) {
		if !protected[id] {
			n, err := r.Meta.DeleteChangesets(ctx, []object.ID{id}, cutoff.Unix())
			if err != nil {
				return nil, err
			}
			if n == 1 {
				deleted[id.String()] = true
				deletedIDs = append(deletedIDs, id)
				r.changesets.Remove(id)
				continue
			}
			if _, _, err := r.Meta.GetChangeset(ctx, id); errors.Is(err, store.ErrNotFound) {
				continue // gone already
			} else if err != nil {
				return nil, err
			}
		}
		if err := m.changeset(ctx, id); err != nil {
			return nil, err
		}
		for _, p := range all[id].parents {
			protected[p] = true
		}
	}
	res.RemovedChangesets = len(deletedIDs)
	type mapping struct{ ns, key string }
	var deadMappings []mapping
	err = r.Meta.Mappings(ctx, "", func(ns, key, value string) error {
		if deleted[key] || deleted[value] {
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
	for _, c := range candidates {
		id := c.id
		if m.objs[id] {
			continue // marked since the walk
		}
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
		keys := deletedIDs
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

// scanLog reads the log entries after since into out and returns the last
// position read.
func (r *Repo) scanLog(ctx context.Context, since int64, out map[object.ID]*gcChangeset) (int64, error) {
	for {
		entries, err := r.Meta.WAL(ctx, since, 10000)
		if err != nil {
			return since, err
		}
		if len(entries) == 0 {
			return since, nil
		}
		for _, e := range entries {
			c, err := object.DecodeChangeset(e.Raw)
			if err != nil {
				return since, fmt.Errorf("changeset %s: %w", e.ID.Short(), err)
			}
			out[e.ID] = &gcChangeset{parents: c.Parents, created: e.Created}
			since = e.Seq
		}
	}
}

// childrenFirst orders dead changesets so that each comes before its
// parents.
func childrenFirst(dead []object.ID, all map[object.ID]*gcChangeset) []object.ID {
	isDead := make(map[object.ID]bool, len(dead))
	for _, id := range dead {
		isDead[id] = true
	}
	children := map[object.ID]int{}
	for _, id := range dead {
		for _, p := range all[id].parents {
			if isDead[p] {
				children[p]++
			}
		}
	}
	var ready, out []object.ID
	for _, id := range dead {
		if children[id] == 0 {
			ready = append(ready, id)
		}
	}
	for len(ready) > 0 {
		id := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		out = append(out, id)
		for _, p := range all[id].parents {
			if isDead[p] {
				if children[p]--; children[p] == 0 {
					ready = append(ready, p)
				}
			}
		}
	}
	return out
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
