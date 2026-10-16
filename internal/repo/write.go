package repo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/textdiff"
)

// ConflictError lists paths that could not be merged automatically.
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	return "conflict in " + strings.Join(e.Paths, ", ")
}

type CreateRequest struct {
	Parents []object.ID       `json:"parents"`
	Author  string            `json:"author"`
	Time    int64             `json:"time,omitempty"`
	TZ      int               `json:"tz,omitempty"`
	Message string            `json:"message"`
	Changes []object.Change   `json:"changes"`
	Extra   map[string]string `json:"extra,omitempty"`
}

type changeCheckKey struct{}

// WithChangeCheck returns a context under which every changeset created, by
// Create directly or by a rebase, merge or land, first passes each path it
// changes to check. The server uses it to enforce write permissions on
// exactly the paths a new changeset touches.
func WithChangeCheck(ctx context.Context, check func(path string) error) context.Context {
	return context.WithValue(ctx, changeCheckKey{}, check)
}

type Created struct {
	ID   object.ID `json:"id"`
	Tree object.ID `json:"tree"`
	Seq  int64     `json:"seq"`
}

// Create validates and appends a changeset. Its cost scales with the number
// of changed files (times directory depth), not with the size of the
// repository, and it takes no locks: refs are moved separately.
func (r *Repo) Create(ctx context.Context, req CreateRequest) (*Created, error) {
	for _, p := range req.Parents {
		// Ask the log rather than the cache: garbage collection may have
		// removed a changeset this process still remembers.
		if _, _, err := r.Meta.GetChangeset(ctx, p); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: unknown parent %s", ErrInvalid, p.Short())
			}
			return nil, err
		}
	}
	var base object.ID
	if len(req.Parents) > 0 {
		var err error
		if base, err = r.TreeOf(ctx, req.Parents[0]); err != nil {
			return nil, err
		}
	}
	changes, err := r.normalizeChanges(ctx, base, req.Changes)
	if err != nil {
		return nil, err
	}
	if check, ok := ctx.Value(changeCheckKey{}).(func(string) error); ok {
		for _, ch := range changes {
			if err := check(ch.Path); err != nil {
				return nil, err
			}
		}
	}
	tree, err := r.applyToTree(ctx, base, changes)
	if err != nil {
		if pe, ok := asPathError(err); ok {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, pe)
		}
		return nil, err
	}
	if req.Time == 0 {
		req.Time = time.Now().Unix()
	}
	c := &object.Changeset{
		Parents: req.Parents,
		Author:  req.Author,
		Time:    req.Time,
		TZ:      req.TZ,
		Message: req.Message,
		Changes: changes,
		Extra:   req.Extra,
	}
	raw := c.Encode()
	id := object.Hash(object.KindChangeset, raw)
	// The trees just written must be on disk before the log names them.
	if err := store.Sync(r.Objs); err != nil {
		return nil, err
	}
	seq, err := r.Meta.AppendChangeset(ctx, id, raw)
	if err != nil {
		return nil, err
	}
	r.changesets.Add(id, c)
	if err := r.putDerived(ctx, TreeDeriver.Name, id, tree[:]); err != nil {
		return nil, err
	}
	for _, d := range r.Eager {
		if _, err := r.Derive(ctx, d, id); err != nil {
			return nil, err
		}
	}
	return &Created{ID: id, Tree: tree, Seq: seq}, nil
}

func (r *Repo) normalizeChanges(ctx context.Context, base object.ID, in []object.Change) ([]object.Change, error) {
	out := make([]object.Change, 0, len(in))
	seen := map[string]bool{}
	for _, ch := range in {
		if err := object.ValidatePath(ch.Path); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if seen[ch.Path] {
			return nil, fmt.Errorf("%w: %s changed twice", ErrInvalid, ch.Path)
		}
		seen[ch.Path] = true
		if !ch.Deleted() {
			if !object.ValidFileMode(ch.Mode) {
				return nil, fmt.Errorf("%w: %s: bad mode %o", ErrInvalid, ch.Path, ch.Mode)
			}
			stored, err := r.Objs.Get(ctx, ch.Blob)
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s: blob %s was not uploaded", ErrInvalid, ch.Path, ch.Blob.Short())
			}
			if err != nil {
				return nil, err
			}
			if len(stored) == 0 || object.Kind(stored[0]) != object.KindBlob {
				return nil, fmt.Errorf("%w: %s: %s is not a blob", ErrInvalid, ch.Path, ch.Blob.Short())
			}
			if err := store.Touch(ctx, r.Objs, ch.Blob); errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s: blob %s was not uploaded", ErrInvalid, ch.Path, ch.Blob.Short())
			} else if err != nil {
				return nil, err
			}
		}
		old, ok, err := r.LookupPath(ctx, base, ch.Path)
		if err != nil {
			return nil, err
		}
		if ch.Deleted() {
			ch.Blob = object.Zero
		} else if ok && old.Mode == ch.Mode && old.ID == ch.Blob {
			continue // no-op
		}
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// threeWay re-applies changes made on top of baseTree onto oursTree. Paths
// untouched upstream are applied as-is; paths changed on both sides get a
// line-level three-way merge, and anything else is a conflict.
func (r *Repo) threeWay(ctx context.Context, baseTree, oursTree object.ID, changes []object.Change) ([]object.Change, error) {
	var out []object.Change
	var conflicts []string
	for _, ch := range changes {
		eb, hb, err := r.LookupPath(ctx, baseTree, ch.Path)
		if err != nil {
			return nil, err
		}
		eo, ho, err := r.LookupPath(ctx, oursTree, ch.Path)
		if err != nil {
			return nil, err
		}
		if hb == ho && (!hb || (eb.Mode == eo.Mode && eb.ID == eo.ID)) {
			out = append(out, ch)
			continue
		}
		if ch.Deleted() {
			if !ho {
				continue // deleted on both sides
			}
			conflicts = append(conflicts, ch.Path)
			continue
		}
		if ho && eo.Mode == ch.Mode && eo.ID == ch.Blob {
			continue // same result on both sides
		}
		if hb && ho && object.IsRegular(eb.Mode) && object.IsRegular(eo.Mode) && object.IsRegular(ch.Mode) {
			merged, ok, err := r.mergeBlobs(ctx, eb.ID, eo.ID, ch.Blob)
			if err != nil {
				return nil, err
			}
			if ok {
				mode := ch.Mode
				if ch.Mode == eb.Mode {
					mode = eo.Mode
				}
				out = append(out, object.Change{Path: ch.Path, Mode: mode, Blob: merged})
				continue
			}
		}
		conflicts = append(conflicts, ch.Path)
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Paths: conflicts}
	}
	return out, nil
}

func (r *Repo) mergeBlobs(ctx context.Context, base, ours, theirs object.ID) (object.ID, bool, error) {
	var bufs [3][]byte
	for i, id := range []object.ID{base, ours, theirs} {
		b, err := r.ReadBlob(ctx, id)
		if err != nil {
			return object.Zero, false, err
		}
		if textdiff.IsBinary(b) {
			return object.Zero, false, nil
		}
		bufs[i] = b
	}
	merged, ok := textdiff.Merge3(bufs[0], bufs[1], bufs[2])
	if !ok {
		return object.Zero, false, nil
	}
	id, err := r.WriteBlob(ctx, merged)
	return id, err == nil, err
}

// createOrConflict turns structural path errors into conflicts, since during
// rebase and merge they come from upstream changes, not from the caller.
func (r *Repo) createOrConflict(ctx context.Context, req CreateRequest) (*Created, error) {
	c, err := r.Create(ctx, req)
	if pe, ok := asPathError(err); ok {
		return nil, &ConflictError{Paths: []string{pe.Path}}
	}
	return c, err
}

func (r *Repo) rebaseOne(ctx context.Context, id, onto object.ID) (object.ID, error) {
	c, err := r.Changeset(ctx, id)
	if err != nil {
		return object.Zero, err
	}
	if len(c.Parents) != 1 {
		return object.Zero, fmt.Errorf("%w: cannot rebase merge changeset %s", ErrInvalid, id.Short())
	}
	if c.Parents[0] == onto {
		return id, nil
	}
	baseTree, err := r.TreeOf(ctx, c.Parents[0])
	if err != nil {
		return object.Zero, err
	}
	ontoTree, err := r.TreeOf(ctx, onto)
	if err != nil {
		return object.Zero, err
	}
	changes, err := r.threeWay(ctx, baseTree, ontoTree, c.Changes)
	if err != nil {
		return object.Zero, err
	}
	extra := map[string]string{}
	for k, v := range c.Extra {
		extra[k] = v
	}
	extra["rebased_from"] = id.String()
	created, err := r.createOrConflict(ctx, CreateRequest{
		Parents: []object.ID{onto},
		Author:  c.Author,
		Time:    c.Time,
		TZ:      c.TZ,
		Message: c.Message,
		Changes: changes,
		Extra:   extra,
	})
	if err != nil {
		return object.Zero, err
	}
	return created.ID, nil
}

// Rebase moves the stack of changesets between merge-base(head, onto) and
// head on top of onto, entirely on the server, and returns the new head.
func (r *Repo) Rebase(ctx context.Context, head, onto object.ID) (object.ID, error) {
	mb, err := r.MergeBase(ctx, head, onto)
	if err != nil {
		return object.Zero, err
	}
	switch {
	case mb.IsZero():
		return object.Zero, fmt.Errorf("%w: %s and %s share no history", ErrInvalid, head.Short(), onto.Short())
	case mb == head:
		return onto, nil
	case mb == onto:
		return head, nil
	}
	gmb, err := r.genInfo(ctx, mb)
	if err != nil {
		return object.Zero, err
	}
	var chain []object.ID
	for x := head; x != mb; {
		c, err := r.Changeset(ctx, x)
		if err != nil {
			return object.Zero, err
		}
		if len(c.Parents) != 1 {
			return object.Zero, fmt.Errorf("%w: cannot rebase across merge changeset %s", ErrInvalid, x.Short())
		}
		chain = append(chain, x)
		x = c.Parents[0]
		if g, err := r.genInfo(ctx, x); err != nil {
			return object.Zero, err
		} else if g.gen < gmb.gen {
			return object.Zero, fmt.Errorf("%w: merge base is not on %s's first-parent line", ErrInvalid, head.Short())
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if onto, err = r.rebaseOne(ctx, chain[i], onto); err != nil {
			return object.Zero, err
		}
	}
	return onto, nil
}

// Merge creates a two-parent changeset joining theirs into ours. Its changes
// are listed relative to ours, its first parent.
func (r *Repo) Merge(ctx context.Context, ours, theirs object.ID, author, message string) (*Created, error) {
	mb, err := r.MergeBase(ctx, ours, theirs)
	if err != nil {
		return nil, err
	}
	if mb == theirs {
		return nil, fmt.Errorf("%w: %s is already merged", ErrInvalid, theirs.Short())
	}
	var baseTree object.ID
	if !mb.IsZero() {
		if baseTree, err = r.TreeOf(ctx, mb); err != nil {
			return nil, err
		}
	}
	theirsTree, err := r.TreeOf(ctx, theirs)
	if err != nil {
		return nil, err
	}
	oursTree, err := r.TreeOf(ctx, ours)
	if err != nil {
		return nil, err
	}
	var theirChanges []object.Change
	err = r.DiffTrees(ctx, baseTree, theirsTree, "", func(d FileDiff) error {
		ch := object.Change{Path: d.Path}
		if d.New != nil {
			ch.Mode, ch.Blob = d.New.Mode, d.New.ID
		}
		theirChanges = append(theirChanges, ch)
		return nil
	})
	if err != nil {
		return nil, err
	}
	changes, err := r.threeWay(ctx, baseTree, oursTree, theirChanges)
	if err != nil {
		return nil, err
	}
	return r.createOrConflict(ctx, CreateRequest{
		Parents: []object.ID{ours, theirs},
		Author:  author,
		Message: message,
		Changes: changes,
	})
}

type LandResult struct {
	Landed   object.ID `json:"landed"`
	Previous object.ID `json:"previous"`
	Attempts int       `json:"attempts"`
	Rebased  bool      `json:"rebased"`
}

// Land puts head on top of ref without any global lock: rebase onto the
// current tip, then compare-and-swap the ref. Losing the race just means
// rebasing onto the new tip and trying again.
func (r *Repo) Land(ctx context.Context, head object.ID, ref string) (*LandResult, error) {
	const maxAttempts = 200
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tip, err := r.Meta.GetRef(ctx, ref)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		res := &LandResult{Previous: tip, Attempts: attempt}
		next := head
		if !tip.IsZero() {
			if ok, err := r.IsAncestor(ctx, head, tip); err != nil {
				return nil, err
			} else if ok {
				res.Landed = tip
				return res, nil // already landed
			}
			if ok, err := r.IsAncestor(ctx, tip, head); err != nil {
				return nil, err
			} else if !ok {
				if next, err = r.Rebase(ctx, head, tip); err != nil {
					return nil, err
				}
				res.Rebased = true
			}
		}
		err = r.Meta.CASRef(ctx, ref, tip, next)
		if err == nil {
			res.Landed = next
			return res, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not land on %s after %d attempts", ref, maxAttempts)
}
