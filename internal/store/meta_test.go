package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
)

// TestMigrateCreated opens a store made before changesets had a created
// column: existing entries must count as created now, not at the epoch.
func TestMigrateCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE changesets (seq INTEGER PRIMARY KEY AUTOINCREMENT, id BLOB NOT NULL UNIQUE, data BLOB NOT NULL);
		INSERT INTO changesets (id, data) VALUES (x'01', '{}')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wal, err := s.WAL(context.Background(), 0, 10)
	if err != nil || len(wal) != 1 {
		t.Fatalf("wal = %v, %v", wal, err)
	}
	if age := time.Now().Unix() - wal[0].Created; age < 0 || age > 60 {
		t.Fatalf("migrated entry has created %d", wal[0].Created)
	}
	s.Close()
	again, err := OpenSQLite(path) // migrating twice is a no-op
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestDeleteChangesetsRespectsCutoff(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	a, b := object.Hash(object.KindChangeset, []byte("a")), object.Hash(object.KindChangeset, []byte("b"))
	seqA, _ := s.AppendChangeset(ctx, a, []byte("a"))
	s.AppendChangeset(ctx, b, []byte("b"))
	if seq, err := s.AppendChangeset(ctx, a, []byte("a")); err != nil || seq != seqA {
		t.Fatalf("re-append = %d, %v; want seq %d", seq, err, seqA)
	}
	now := time.Now().Unix()
	if n, err := s.DeleteChangesets(ctx, []object.ID{a, b}, now-60); err != nil || n != 0 {
		t.Fatalf("deleted %d entries newer than the cutoff (%v)", n, err)
	}
	if n, err := s.DeleteChangesets(ctx, []object.ID{a}, now+1); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	wal, _ := s.WAL(ctx, 0, 10)
	if len(wal) != 1 || wal[0].ID != b || wal[0].Seq != seqA+1 {
		t.Fatalf("wal after delete = %+v", wal)
	}
}
