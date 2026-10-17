package repo

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/textdiff"
)

// Deriver defines one type of derived data. Every type is defined
// recursively: its value for a key is computed from its values for the key's
// parents. One engine (Derive) handles memoization, ordering and storage for
// all of them, so adding a new index is just adding a Deriver, and whether a
// type is computed eagerly or lazily is a per-type choice.
type Deriver struct {
	Name    string
	Parents func(ctx context.Context, r *Repo, key object.ID) ([]object.ID, error)
	Compute func(ctx context.Context, r *Repo, key object.ID, parents [][]byte) ([]byte, error)
}

func changesetParents(ctx context.Context, r *Repo, key object.ID) ([]object.ID, error) {
	c, err := r.Changeset(ctx, key)
	if err != nil {
		return nil, err
	}
	return c.Parents, nil
}

func (r *Repo) getDerived(ctx context.Context, kind string, key object.ID) ([]byte, bool, error) {
	k := derivedKey{kind, key}
	if v, ok := r.derived.Get(k); ok {
		return v, true, nil
	}
	v, err := r.Meta.GetDerived(ctx, kind, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	r.derived.Add(k, v)
	return v, true, nil
}

func (r *Repo) putDerived(ctx context.Context, kind string, key object.ID, v []byte) error {
	// Objects a value names (unodes, say) must be on disk before it is stored.
	if err := store.Sync(r.Objs); err != nil {
		return err
	}
	if err := r.Meta.PutDerived(ctx, kind, key, v); err != nil {
		return err
	}
	r.derived.Add(derivedKey{kind, key}, v)
	return nil
}

// Derive returns d's value for key, computing any missing ancestors first.
// It walks iteratively, so arbitrarily long histories don't overflow the stack.
func (r *Repo) Derive(ctx context.Context, d *Deriver, key object.ID) ([]byte, error) {
	if v, ok, err := r.getDerived(ctx, d.Name, key); err != nil || ok {
		return v, err
	}
	return r.flights.do(ctx, d.Name+" "+key.String(), func(ctx context.Context) ([]byte, error) {
		type frame struct {
			key      object.ID
			parents  []object.ID
			expanded bool
		}
		stack := []frame{{key: key}}
		for len(stack) > 0 {
			top := len(stack) - 1
			f := stack[top]
			if _, ok, err := r.getDerived(ctx, d.Name, f.key); err != nil {
				return nil, err
			} else if ok {
				stack = stack[:top]
				continue
			}
			if !f.expanded {
				ps, err := d.Parents(ctx, r, f.key)
				if err != nil {
					return nil, err
				}
				f.parents, f.expanded = ps, true
				stack[top] = f
			}
			vals := make([][]byte, len(f.parents))
			pushed := false
			for i, p := range f.parents {
				v, ok, err := r.getDerived(ctx, d.Name, p)
				if err != nil {
					return nil, err
				}
				if !ok {
					stack = append(stack, frame{key: p})
					pushed = true
				}
				vals[i] = v
			}
			if pushed {
				continue
			}
			v, err := d.Compute(ctx, r, f.key, vals)
			if err != nil {
				return nil, fmt.Errorf("derive %s for %s: %w", d.Name, f.key.Short(), err)
			}
			if err := r.putDerived(ctx, d.Name, f.key, v); err != nil {
				return nil, err
			}
			stack = stack[:top]
		}
		v, _, err := r.getDerived(ctx, d.Name, key)
		return v, err
	})
}

func idValue(v []byte) object.ID {
	var id object.ID
	copy(id[:], v)
	return id
}

// TreeDeriver: the root tree of a changeset = its first parent's root tree
// with the changeset's file changes applied.
var TreeDeriver = &Deriver{
	Name:    "tree",
	Parents: changesetParents,
	Compute: func(ctx context.Context, r *Repo, key object.ID, parents [][]byte) ([]byte, error) {
		c, err := r.Changeset(ctx, key)
		if err != nil {
			return nil, err
		}
		var base object.ID
		if len(parents) > 0 {
			base = idValue(parents[0])
		}
		id, err := r.applyToTree(ctx, base, c.Changes)
		return id[:], err
	},
}

// UnodeDeriver: the root unode of a changeset. Each file or directory that the
// changeset touches gets a new unode linked to the changeset and to the
// previous version at the same path (first-parent history).
var UnodeDeriver = &Deriver{
	Name:    "unode",
	Parents: changesetParents,
	Compute: func(ctx context.Context, r *Repo, key object.ID, parents [][]byte) ([]byte, error) {
		c, err := r.Changeset(ctx, key)
		if err != nil {
			return nil, err
		}
		var base object.ID
		if len(parents) > 0 {
			base = idValue(parents[0])
		}
		id, _, err := applyEdits(ctx, unodeOps{r, key}, base, buildEdits(c.Changes), "")
		return id[:], err
	},
}

// GenDeriver: generation number plus a first-parent skip list. skip[k] is the
// ancestor 2^k first-parent steps away, so ancestry checks and "rev~N" jump
// in O(log n) instead of walking the log.
var GenDeriver = &Deriver{Name: "gen", Parents: changesetParents}

// Assigned in init: computeGen reads ancestors' values through GenDeriver.
func init() { GenDeriver.Compute = computeGen }

func computeGen(ctx context.Context, r *Repo, key object.ID, parents [][]byte) ([]byte, error) {
	c, err := r.Changeset(ctx, key)
	if err != nil {
		return nil, err
	}
	var gen uint64
	for _, p := range parents {
		if g := decodeGen(p).gen; g > gen {
			gen = g
		}
	}
	gi := genInfo{gen: gen + 1}
	if len(c.Parents) > 0 {
		gi.skips = append(gi.skips, c.Parents[0])
		for k := 1; ; k++ {
			prev, err := r.genInfo(ctx, gi.skips[k-1])
			if err != nil {
				return nil, err
			}
			if len(prev.skips) < k {
				break
			}
			gi.skips = append(gi.skips, prev.skips[k-1])
		}
	}
	return gi.encode(), nil
}

type genInfo struct {
	gen   uint64
	skips []object.ID
}

func (g genInfo) encode() []byte {
	b := make([]byte, 8+len(g.skips)*object.IDSize)
	binary.BigEndian.PutUint64(b, g.gen)
	for i, s := range g.skips {
		copy(b[8+i*object.IDSize:], s[:])
	}
	return b
}

func decodeGen(b []byte) genInfo {
	g := genInfo{gen: binary.BigEndian.Uint64(b[:8])}
	for rest := b[8:]; len(rest) >= object.IDSize; rest = rest[object.IDSize:] {
		g.skips = append(g.skips, idValue(rest))
	}
	return g
}

func (r *Repo) genInfo(ctx context.Context, id object.ID) (genInfo, error) {
	v, err := r.Derive(ctx, GenDeriver, id)
	if err != nil {
		return genInfo{}, err
	}
	return decodeGen(v), nil
}

// BlameDeriver is keyed by file unode, not changeset: its parent is the
// previous version of the same file. Lines that survive a diff against the
// previous version keep their attribution; new lines belong to the unode's
// linknode. It is lazy by default: computed the first time someone asks.
var BlameDeriver = &Deriver{
	Name: "blame",
	Parents: func(ctx context.Context, r *Repo, key object.ID) ([]object.ID, error) {
		u, err := r.ReadUnode(ctx, key)
		if err != nil {
			return nil, err
		}
		if u.Dir {
			return nil, fmt.Errorf("%w: blame of a directory", ErrInvalid)
		}
		if len(u.Parents) > 0 {
			return u.Parents[:1], nil
		}
		return nil, nil
	},
	Compute: func(ctx context.Context, r *Repo, key object.ID, parents [][]byte) ([]byte, error) {
		u, err := r.ReadUnode(ctx, key)
		if err != nil {
			return nil, err
		}
		content, err := r.ReadBlob(ctx, u.Content)
		if err != nil {
			return nil, err
		}
		if !object.IsRegular(u.Mode) || textdiff.IsBinary(content) {
			return json.Marshal(blameValue{Binary: true})
		}
		lines := textdiff.SplitLines(content)
		attr := make([]object.ID, len(lines))
		for i := range attr {
			attr[i] = u.Linknode
		}
		if len(parents) > 0 {
			var pb blameValue
			if err := json.Unmarshal(parents[0], &pb); err != nil {
				return nil, err
			}
			pu, err := r.ReadUnode(ctx, u.Parents[0])
			if err != nil {
				return nil, err
			}
			if !pb.Binary {
				prevContent, err := r.ReadBlob(ctx, pu.Content)
				if err != nil {
					return nil, err
				}
				prevAttr := pb.expand()
				prevLines := textdiff.SplitLines(prevContent)
				pos, posNew := 0, 0
				copyEqual := func(n int) {
					for k := 0; k < n; k++ {
						if pos+k < len(prevAttr) {
							attr[posNew+k] = prevAttr[pos+k]
						}
					}
				}
				for _, h := range textdiff.Lines(prevLines, lines) {
					copyEqual(h.A0 - pos)
					pos, posNew = h.A1, h.B1
				}
				copyEqual(len(lines) - posNew)
			}
		}
		return json.Marshal(compressBlame(attr))
	},
}

type blameValue struct {
	Binary     bool        `json:"binary,omitempty"`
	Changesets []object.ID `json:"changesets,omitempty"`
	Runs       [][2]int    `json:"runs,omitempty"` // [line count, changeset index]
}

func compressBlame(attr []object.ID) blameValue {
	var v blameValue
	index := map[object.ID]int{}
	for _, id := range attr {
		i, ok := index[id]
		if !ok {
			i = len(v.Changesets)
			index[id] = i
			v.Changesets = append(v.Changesets, id)
		}
		if n := len(v.Runs); n > 0 && v.Runs[n-1][1] == i {
			v.Runs[n-1][0]++
		} else {
			v.Runs = append(v.Runs, [2]int{1, i})
		}
	}
	return v
}

func (v blameValue) expand() []object.ID {
	var out []object.ID
	for _, run := range v.Runs {
		for k := 0; k < run[0]; k++ {
			out = append(out, v.Changesets[run[1]])
		}
	}
	return out
}

// Derivers lists every derived type by name.
var Derivers = map[string]*Deriver{
	TreeDeriver.Name:  TreeDeriver,
	UnodeDeriver.Name: UnodeDeriver,
	GenDeriver.Name:   GenDeriver,
	BlameDeriver.Name: BlameDeriver,
}

func (r *Repo) TreeOf(ctx context.Context, id object.ID) (object.ID, error) {
	v, err := r.Derive(ctx, TreeDeriver, id)
	if err != nil {
		return object.Zero, err
	}
	return idValue(v), nil
}

func (r *Repo) UnodeOf(ctx context.Context, id object.ID) (object.ID, error) {
	v, err := r.Derive(ctx, UnodeDeriver, id)
	if err != nil {
		return object.Zero, err
	}
	return idValue(v), nil
}

// flightGroup collapses concurrent computations of the same key. The
// computation runs on its own goroutine with a context that keeps the first
// caller's values but not its cancellation, so a caller that gives up (a
// client hanging up during a large blame) doesn't fail the others waiting for
// the same value. Each caller stops waiting when its own context is done.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done chan struct{} // closed once val and err are set
	val  []byte
	err  error
}

func (g *flightGroup) do(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flight{}
	}
	f, ok := g.m[key]
	if !ok {
		f = &flight{done: make(chan struct{})}
		g.m[key] = f
		go g.run(context.WithoutCancel(ctx), key, f, fn)
	}
	g.mu.Unlock()
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run computes f's value and always releases its waiters and its key, even
// if fn panics: the panic becomes every waiter's error, and the next call
// for the key computes it afresh.
func (g *flightGroup) run(ctx context.Context, key string, f *flight, fn func(context.Context) ([]byte, error)) {
	defer func() {
		if p := recover(); p != nil {
			f.val, f.err = nil, fmt.Errorf("derive %s: panic: %v", key, p)
		}
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		close(f.done)
	}()
	f.val, f.err = fn(ctx)
}
