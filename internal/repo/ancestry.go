package repo

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
)

// IsAncestor reports whether a is b or an ancestor of b.
func (r *Repo) IsAncestor(ctx context.Context, a, b object.ID) (bool, error) {
	if a == b {
		return true, nil
	}
	ga, err := r.genInfo(ctx, a)
	if err != nil {
		return false, err
	}
	// Fast path: binary-lift along b's first-parent chain to the oldest
	// changeset whose generation is still >= a's. On trunk-based history this
	// answers in O(log n) lookups.
	x := b
	for x != a {
		gx, err := r.genInfo(ctx, x)
		if err != nil {
			return false, err
		}
		if gx.gen <= ga.gen {
			break
		}
		jumped := false
		for k := len(gx.skips) - 1; k >= 0; k-- {
			gs, err := r.genInfo(ctx, gx.skips[k])
			if err != nil {
				return false, err
			}
			if gs.gen >= ga.gen {
				x, jumped = gx.skips[k], true
				break
			}
		}
		if !jumped {
			break
		}
	}
	if x == a {
		return true, nil
	}
	// Slow path: search all parents, pruning anything older than a.
	seen := map[object.ID]bool{b: true}
	queue := []object.ID{b}
	for len(queue) > 0 {
		x := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		c, err := r.Changeset(ctx, x)
		if err != nil {
			return false, err
		}
		for _, p := range c.Parents {
			if p == a {
				return true, nil
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			gp, err := r.genInfo(ctx, p)
			if err != nil {
				return false, err
			}
			if gp.gen > ga.gen {
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}

type genItem struct {
	id  object.ID
	gen uint64
}

type genHeap []genItem

func (h genHeap) Len() int { return len(h) }
func (h genHeap) Less(i, j int) bool {
	if h[i].gen != h[j].gen {
		return h[i].gen > h[j].gen
	}
	return bytes.Compare(h[i].id[:], h[j].id[:]) < 0
}
func (h genHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *genHeap) Push(x any)   { *h = append(*h, x.(genItem)) }
func (h *genHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}

// MergeBase returns a best common ancestor of a and b (zero if none). Nodes
// are visited in decreasing generation order, so the first node reached from
// both sides has no common ancestor above it.
func (r *Repo) MergeBase(ctx context.Context, a, b object.ID) (object.ID, error) {
	if ok, err := r.IsAncestor(ctx, a, b); err != nil || ok {
		return a, err
	}
	if ok, err := r.IsAncestor(ctx, b, a); err != nil || ok {
		return b, err
	}
	flags := map[object.ID]uint8{a: 1, b: 2}
	h := &genHeap{}
	for _, id := range []object.ID{a, b} {
		g, err := r.genInfo(ctx, id)
		if err != nil {
			return object.Zero, err
		}
		heap.Push(h, genItem{id, g.gen})
	}
	for h.Len() > 0 {
		it := heap.Pop(h).(genItem)
		f := flags[it.id]
		if f == 3 {
			return it.id, nil
		}
		c, err := r.Changeset(ctx, it.id)
		if err != nil {
			return object.Zero, err
		}
		for _, p := range c.Parents {
			if flags[p]&f == f {
				continue
			}
			flags[p] |= f
			g, err := r.genInfo(ctx, p)
			if err != nil {
				return object.Zero, err
			}
			heap.Push(h, genItem{p, g.gen})
		}
	}
	return object.Zero, nil
}

// NthAncestor follows n first-parent steps using the skip list, taking the
// low bits of n first. A changeset at depth D has skip levels for every
// 2^k <= D, so the needed level always exists when n <= D.
func (r *Repo) NthAncestor(ctx context.Context, id object.ID, n int) (object.ID, error) {
	for k := 0; n > 0; k++ {
		if n&1 == 1 {
			g, err := r.genInfo(ctx, id)
			if err != nil {
				return object.Zero, err
			}
			if k >= len(g.skips) {
				return object.Zero, fmt.Errorf("%w: history is not that long", ErrInvalid)
			}
			id = g.skips[k]
		}
		n >>= 1
	}
	return id, nil
}

// Resolve turns a revision into a changeset ID. Accepted forms: a ref name
// or a full or abbreviated (>= 4 hex chars) changeset ID, followed by any
// number of suffixes applied left to right, as in git:
//
//	~N  the Nth first-parent ancestor (~ alone is ~1)
//	^N  the Nth parent (^ alone is ^1, ^0 is the changeset itself)
//
// so main~2^2~1 is the first parent of the second parent of main~2.
func (r *Repo) Resolve(ctx context.Context, rev string) (object.ID, error) {
	base, suffixes := rev, ""
	if i := strings.IndexAny(rev, "~^"); i >= 0 {
		base, suffixes = rev[:i], rev[i:]
	}
	steps, err := parseRevSuffixes(suffixes)
	if err != nil {
		return object.Zero, fmt.Errorf("%w: bad revision %q: %v", ErrInvalid, rev, err)
	}
	id, err := r.resolveBase(ctx, base, rev)
	if err != nil {
		return object.Zero, err
	}
	for _, s := range steps {
		if s.op == '~' {
			if id, err = r.NthAncestor(ctx, id, s.n); err != nil {
				return object.Zero, fmt.Errorf("revision %q: %w", rev, err)
			}
			continue
		}
		if s.n == 0 {
			continue
		}
		c, err := r.Changeset(ctx, id)
		if err != nil {
			return object.Zero, err
		}
		if s.n > len(c.Parents) {
			return object.Zero, fmt.Errorf("%w: revision %q: %s has %d parent(s), not %d", ErrInvalid, rev, id.Short(), len(c.Parents), s.n)
		}
		id = c.Parents[s.n-1]
	}
	return id, nil
}

type revStep struct {
	op byte // '~' or '^'
	n  int
}

// parseRevSuffixes splits "~2^2~" into [{~ 2} {^ 2} {~ 1}].
func parseRevSuffixes(s string) ([]revStep, error) {
	var steps []revStep
	for len(s) > 0 {
		op := s[0]
		if op != '~' && op != '^' {
			return nil, fmt.Errorf("unexpected %q", s)
		}
		s = s[1:]
		j := 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		n := 1
		if j > 0 {
			v, err := strconv.Atoi(s[:j])
			if err != nil {
				return nil, err
			}
			n = v
		}
		steps = append(steps, revStep{op, n})
		s = s[j:]
	}
	return steps, nil
}

// resolveBase resolves a ref name or changeset ID prefix (rev is the whole
// revision, for error messages).
func (r *Repo) resolveBase(ctx context.Context, base, rev string) (object.ID, error) {
	id, err := r.Meta.GetRef(ctx, base)
	switch {
	case err == nil:
	case !errors.Is(err, store.ErrNotFound):
		return object.Zero, err
	case len(base) >= 4 && isHex(base):
		id, err = r.Meta.ResolvePrefix(ctx, base)
		if errors.Is(err, store.ErrNotFound) {
			return object.Zero, fmt.Errorf("unknown revision %q: %w", rev, store.ErrNotFound)
		}
		if errors.Is(err, store.ErrAmbiguous) {
			return object.Zero, fmt.Errorf("%w: revision %q is ambiguous", ErrInvalid, rev)
		}
		if err != nil {
			return object.Zero, err
		}
	default:
		return object.Zero, fmt.Errorf("unknown revision %q: %w", rev, store.ErrNotFound)
	}
	return id, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
