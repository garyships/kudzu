package repo

import (
	"context"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
)

// syncCheck is an object store that batches durability like a durable FS
// and a metadata store that fails the test if it commits anything while
// objects are still unsynced.
type syncCheck struct {
	store.Objects
	unsynced int
	syncs    int
}

func (s *syncCheck) Put(ctx context.Context, id object.ID, b []byte) error {
	s.unsynced++
	return s.Objects.Put(ctx, id, b)
}

func (s *syncCheck) Sync() error {
	if s.unsynced > 0 {
		s.syncs++
	}
	s.unsynced = 0
	return nil
}

type metaCheck struct {
	store.Meta
	t    *testing.T
	objs *syncCheck
}

func (m *metaCheck) AppendChangeset(ctx context.Context, id object.ID, raw []byte) (int64, error) {
	if m.objs.unsynced > 0 {
		m.t.Errorf("changeset %s appended with %d objects not synced", id.Short(), m.objs.unsynced)
	}
	return m.Meta.AppendChangeset(ctx, id, raw)
}

func (m *metaCheck) PutDerived(ctx context.Context, kind string, key object.ID, v []byte) error {
	if m.objs.unsynced > 0 {
		m.t.Errorf("derived %s for %s stored with %d objects not synced", kind, key.Short(), m.objs.unsynced)
	}
	return m.Meta.PutDerived(ctx, kind, key, v)
}

// TestObjectsSyncedBeforeMetadata: with a store that makes writes durable in
// batches, everything a changeset or derived value names is synced before
// the metadata that names it is committed, and in few syncs.
func TestObjectsSyncedBeforeMetadata(t *testing.T) {
	base := newTestRepo(t)
	objs := &syncCheck{Objects: base.Objs}
	r := New(&metaCheck{Meta: base.Meta, t: t, objs: objs}, objs)
	ctx := context.Background()
	c1 := commit(t, r, nil, "one", files{"a/b/c.txt": "1\n", "d.txt": "2\n"})
	c2 := commit(t, r, []object.ID{c1}, "two", files{"a/b/c.txt": "3\n"})
	if _, err := r.Blame(ctx, c2, "a/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if objs.syncs == 0 || objs.syncs > 6 {
		t.Fatalf("%d syncs for two small changesets", objs.syncs)
	}
}
