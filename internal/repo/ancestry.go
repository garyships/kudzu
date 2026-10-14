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

// Resolve turns a revision into a changeset ID. Accepted forms: a ref name,
// a full or abbreviated (>= 4 hex chars) changeset ID, either followed by
// ~N for the Nth first-parent ancestor.
func (r *Repo) Resolve(ctx context.Context, rev string) (object.ID, error) {
	base, n := rev, 0
	if i := strings.LastIndexByte(rev, '~'); i >= 0 {
		base = rev[:i]
		n = 1
		if s := rev[i+1:]; s != "" {
			v, err := strconv.Atoi(s)
			if err != nil || v < 0 {
				return object.Zero, fmt.Errorf("%w: bad revision %q", ErrInvalid, rev)
			}
			n = v
		}
	}
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
	if n > 0 {
		return r.NthAncestor(ctx, id, n)
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
