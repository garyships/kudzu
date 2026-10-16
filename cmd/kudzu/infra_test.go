package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
)

// unreadable fails to read one object, like a disk with a bad sector.
type unreadable struct {
	store.Objects
	id object.ID
}

func (u *unreadable) Get(ctx context.Context, id object.ID) ([]byte, error) {
	if id == u.id {
		return nil, errors.New("read error")
	}
	return u.Objects.Get(ctx, id)
}

// TestSnapshotInterrupted: when the server fails partway through a
// snapshot, kudzu snapshot fails and leaves no partial file to seed from.
func TestSnapshotInterrupted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta, err := store.OpenSQLite(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	objs := &unreadable{Objects: store.NewMem(4 << 20)}
	r := repo.New(meta, objs)
	srv := httptest.NewServer(server.New(r, log.New(io.Discard, "", 0)).Handler())
	defer srv.Close()

	big := make([]byte, 256<<10) // more than the server buffers
	rand.Read(big)
	var changes []object.Change
	for i, content := range [][]byte{big, []byte("b\n")} {
		id, err := r.WriteBlob(ctx, content)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, object.Change{Path: string(rune('a' + i)), Mode: object.ModeFile, Blob: id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Author: "a <a@example.com>", Message: "m", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	objs.id = changes[1].Blob

	out := filepath.Join(dir, "seed.frames")
	if err := cmdSnapshot(ctx, []string{"--remote", srv.URL, "-o", out}); err == nil {
		t.Fatal("snapshot succeeded although the server could not read an object")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial snapshot left behind: %v", err)
	}
}
