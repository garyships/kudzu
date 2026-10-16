// Package repo is the server-side core: reading and writing objects and
// changesets, the derived-data engine, history queries, and server-side
// commit, rebase, merge and land.
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/garyships/kudzu/internal/lru"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
)

// ErrInvalid marks a request the caller must fix (bad path, unknown parent...).
var ErrInvalid = errors.New("invalid request")

type Repo struct {
	Meta store.Meta
	Objs store.Objects

	// Eager lists the derived types computed as soon as a changeset is
	// created. Everything else is computed on first use.
	Eager []*Deriver

	changesets *lru.Cache[object.ID, *object.Changeset]
	trees      *lru.Cache[object.ID, *object.Tree]
	unodes     *lru.Cache[object.ID, *object.Unode]
	derived    *lru.Cache[derivedKey, []byte]
	flights    flightGroup
}

type derivedKey struct {
	kind string
	id   object.ID
}

func New(meta store.Meta, objs store.Objects) *Repo {
	return &Repo{
		Meta:       meta,
		Objs:       objs,
		Eager:      []*Deriver{GenDeriver, UnodeDeriver},
		changesets: lru.New[object.ID, *object.Changeset](100_000),
		trees:      lru.New[object.ID, *object.Tree](200_000),
		unodes:     lru.New[object.ID, *object.Unode](200_000),
		derived:    lru.New[derivedKey, []byte](500_000),
	}
}

func (r *Repo) Changeset(ctx context.Context, id object.ID) (*object.Changeset, error) {
	if c, ok := r.changesets.Get(id); ok {
		return c, nil
	}
	raw, _, err := r.Meta.GetChangeset(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("changeset %s: %w", id.Short(), store.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	c, err := object.DecodeChangeset(raw)
	if err != nil {
		return nil, err
	}
	r.changesets.Add(id, c)
	return c, nil
}

func (r *Repo) readObject(ctx context.Context, id object.ID, want object.Kind) ([]byte, error) {
	stored, err := r.Objs.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s %s: %w", want, id.Short(), store.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	kind, content, err := object.Decode(stored)
	if err != nil {
		return nil, err
	}
	if kind != want {
		return nil, fmt.Errorf("object %s is a %s, not a %s", id.Short(), kind, want)
	}
	return content, nil
}

// writeObject stores an object unless it exists already, in which case it is
// touched so that garbage collection keeps it now that it is reused.
func (r *Repo) writeObject(ctx context.Context, kind object.Kind, content []byte) (object.ID, error) {
	if len(content) > object.MaxSize {
		// No tier would decode it.
		return object.Zero, fmt.Errorf("%w: %s of %d bytes is over the %d-byte object limit", ErrInvalid, kind, len(content), object.MaxSize)
	}
	id := object.Hash(kind, content)
	if err := store.Touch(ctx, r.Objs, id); err == nil {
		return id, nil
	}
	return id, r.Objs.Put(ctx, id, object.Encode(kind, content))
}

func (r *Repo) ReadBlob(ctx context.Context, id object.ID) ([]byte, error) {
	return r.readObject(ctx, id, object.KindBlob)
}

func (r *Repo) WriteBlob(ctx context.Context, content []byte) (object.ID, error) {
	return r.writeObject(ctx, object.KindBlob, content)
}

func (r *Repo) ReadTree(ctx context.Context, id object.ID) (*object.Tree, error) {
	if id.IsZero() {
		return &object.Tree{}, nil
	}
	if t, ok := r.trees.Get(id); ok {
		return t, nil
	}
	b, err := r.readObject(ctx, id, object.KindTree)
	if err != nil {
		return nil, err
	}
	t, err := object.DecodeTree(b)
	if err != nil {
		return nil, err
	}
	r.trees.Add(id, t)
	return t, nil
}

func (r *Repo) WriteTree(ctx context.Context, t *object.Tree) (object.ID, error) {
	id, err := r.writeObject(ctx, object.KindTree, t.Encode())
	if err == nil {
		r.trees.Add(id, t)
	}
	return id, err
}

func (r *Repo) ReadUnode(ctx context.Context, id object.ID) (*object.Unode, error) {
	if u, ok := r.unodes.Get(id); ok {
		return u, nil
	}
	b, err := r.readObject(ctx, id, object.KindUnode)
	if err != nil {
		return nil, err
	}
	u, err := object.DecodeUnode(b)
	if err != nil {
		return nil, err
	}
	r.unodes.Add(id, u)
	return u, nil
}

func (r *Repo) WriteUnode(ctx context.Context, u *object.Unode) (object.ID, error) {
	id, err := r.writeObject(ctx, object.KindUnode, u.Encode())
	if err == nil {
		r.unodes.Add(id, u)
	}
	return id, err
}
