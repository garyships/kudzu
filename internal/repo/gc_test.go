package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

// gcRepo is a test repository whose store directory is known, so tests can
// age objects and reopen the store without any in-memory caches.
type gcRepo struct {
	*Repo
	dir  string
	meta store.Meta
}

// newGCRepo keeps objects in a directory, whose mtimes the tests age; the
// metadata store follows KUDZU_TEST_POSTGRES like every repo test.
func newGCRepo(t *testing.T) *gcRepo {
	t.Helper()
	dir := t.TempDir()
	meta := storetest.Meta(t)
	objs, err := store.OpenFS(filepath.Join(dir, "objects"), 0)
	if err != nil {
		t.Fatal(err)
	}
	return &gcRepo{New(meta, &store.Cached{Mem: store.NewMem(64 << 20), Backing: objs}), dir, meta}
}

// fresh opens the same store with empty caches.
func (g *gcRepo) fresh(t *testing.T) *Repo {
	objs, err := store.OpenFS(filepath.Join(g.dir, "objects"), 0)
	if err != nil {
		t.Fatal(err)
	}
	return New(g.meta, objs)
}

func (g *gcRepo) age(t *testing.T, id object.ID, d time.Duration) {
	h := id.String()
	old := time.Now().Add(-d)
	if err := os.Chtimes(filepath.Join(g.dir, "objects", h[:2], h[2:]), old, old); err != nil {
		t.Fatal(err)
	}
}

func (g *gcRepo) hasObject(t *testing.T, id object.ID) bool {
	h := id.String()
	_, err := os.Stat(filepath.Join(g.dir, "objects", h[:2], h[2:]))
	return err == nil
}

func blobID(content string) object.ID { return object.Hash(object.KindBlob, []byte(content)) }

func TestGCRemovesUnreachable(t *testing.T) {
	g := newGCRepo(t)
	ctx := context.Background()
	c1 := commit(t, g.Repo, nil, "one", files{"src/a.txt": "a1\n", "README": "hi\n"})
	c2 := commit(t, g.Repo, []object.ID{c1}, "two", files{"src/a.txt": "a2\n"})
	if err := g.Meta.CASRef(ctx, "main", object.Zero, c2); err != nil {
		t.Fatal(err)
	}
	// An abandoned change that never landed, with content nothing else has.
	lost := commit(t, g.Repo, []object.ID{c1}, "abandoned", files{"src/lost/x.txt": "lost content\n"})
	if err := g.Meta.PutMapping(ctx, "git", "deadbeef", lost.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Blame(ctx, lost, "src/lost/x.txt"); err != nil {
		t.Fatal(err)
	}
	// A blob uploaded for a commit that was never created.
	stray, err := g.WriteBlob(ctx, []byte("stray upload\n"))
	if err != nil {
		t.Fatal(err)
	}
	walBefore, _ := g.Meta.WAL(ctx, 0, 100)

	// Everything is inside a one-hour grace period: nothing goes.
	res, err := g.GC(ctx, GCOptions{Keep: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemovedChangesets != 0 || res.RemovedObjects != 0 {
		t.Fatalf("grace period ignored: %+v", res)
	}

	later := time.Now().Add(2 * time.Hour)
	dry, err := g.GC(ctx, GCOptions{Keep: time.Hour, Now: later, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.RemovedChangesets != 1 || dry.RemovedObjects == 0 || !g.hasObject(t, stray) {
		t.Fatalf("dry run: %+v", dry)
	}

	res, err = g.GC(ctx, GCOptions{Keep: time.Hour, Now: later})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemovedChangesets != 1 || res.RemovedMappings != 1 || res.RemovedDerived == 0 {
		t.Fatalf("gc: %+v", res)
	}
	if res.RemovedObjects != dry.RemovedObjects || res.RemovedBytes != dry.RemovedBytes {
		t.Fatalf("dry run %+v disagrees with gc %+v", dry, res)
	}
	if _, _, err := g.Meta.GetChangeset(ctx, lost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("abandoned changeset survived: %v", err)
	}
	if _, err := g.Meta.GetMapping(ctx, "git", "deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("mapping to a removed changeset survived")
	}
	for _, id := range []object.ID{stray, blobID("lost content\n")} {
		if g.hasObject(t, id) {
			t.Fatalf("unreachable blob %s survived", id.Short())
		}
	}
	if _, err := g.Meta.GetDerived(ctx, "tree", lost); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("derived data of a removed changeset survived")
	}
	// Positions in the log are stable: survivors keep their seq.
	walAfter, _ := g.Meta.WAL(ctx, 0, 100)
	if len(walAfter) != len(walBefore)-1 {
		t.Fatalf("wal has %d entries, want %d", len(walAfter), len(walBefore)-1)
	}
	for i, e := range walAfter {
		if e.Seq != walBefore[i].Seq || e.ID != walBefore[i].ID {
			t.Fatalf("wal entry %d moved: %d %s", i, e.Seq, e.ID.Short())
		}
	}

	// Everything main needs is intact, read through a repo with no caches.
	r := g.fresh(t)
	if got := snapshot(t, r, c2); !equalFiles(got, files{"src/a.txt": "a2\n", "README": "hi\n"}) {
		t.Fatalf("main after gc = %v", got)
	}
	if log, err := r.PathLog(ctx, c2, "src/a.txt", 10); err != nil || len(log) != 2 {
		t.Fatalf("path log after gc: %v %v", log, err)
	}
	if bl, err := r.Blame(ctx, c2, "src/a.txt"); err != nil || len(bl) != 1 || bl[0].Changeset != c2 {
		t.Fatalf("blame after gc: %+v %v", bl, err)
	}
	if again, err := r.GC(ctx, GCOptions{Keep: time.Hour, Now: later}); err != nil || again.RemovedObjects != 0 || again.RemovedChangesets != 0 {
		t.Fatalf("second gc should find nothing: %+v %v", again, err)
	}

	// The repo that ran gc no longer trusts its caches for removed data.
	if _, err := g.Create(ctx, CreateRequest{Parents: []object.ID{lost}, Message: "on top of removed"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("building on a removed changeset should fail, got %v", err)
	}
}

// TestGCKeepsRecentAndTouched: an old object is safe once something vouches
// for it again (the server answering /objects/missing, or a changeset reusing
// it), and anything a ref reaches is kept however old.
func TestGCKeepsRecentAndTouched(t *testing.T) {
	g := newGCRepo(t)
	ctx := context.Background()
	c1 := commit(t, g.Repo, nil, "one", files{"a": "kept\n"})
	if err := g.Meta.CASRef(ctx, "main", object.Zero, c1); err != nil {
		t.Fatal(err)
	}
	old, _ := g.WriteBlob(ctx, []byte("old but touched\n"))
	reused, _ := g.WriteBlob(ctx, []byte("old but reused\n"))
	gone, _ := g.WriteBlob(ctx, []byte("old and forgotten\n"))
	for _, id := range []object.ID{old, reused, gone, blobID("kept\n")} {
		g.age(t, id, 3*time.Hour)
	}
	if err := store.Touch(ctx, g.Objs, old); err != nil {
		t.Fatal(err)
	}
	c2 := commit(t, g.Repo, []object.ID{c1}, "reuse", files{"b": "old but reused\n"})
	if err := g.Meta.CASRef(ctx, "main", c1, c2); err != nil {
		t.Fatal(err)
	}
	res, err := g.GC(ctx, GCOptions{Keep: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.RemovedObjects != 1 || g.hasObject(t, gone) {
		t.Fatalf("expected only the forgotten blob to go: %+v", res)
	}
	for _, id := range []object.ID{old, reused, blobID("kept\n")} {
		if !g.hasObject(t, id) {
			t.Fatalf("%s was removed", id.Short())
		}
	}
	// The memory layer must not vouch for an object that is gone from disk.
	if err := store.Touch(ctx, g.Objs, gone); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("touch of a removed object = %v", err)
	}
}
