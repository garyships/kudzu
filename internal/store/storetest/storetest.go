// Package storetest opens the storage backends that tests run against. By
// default that is SQLite and a local directory. KUDZU_TEST_POSTGRES (a
// database URL) and KUDZU_TEST_S3 (an object store URL such as
// s3://bucket?endpoint=http://127.0.0.1:9000, with credentials in
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY) switch the repo and end-to-end
// tests to shared backends; each test gets its own schema or key prefix and
// cleans it up.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
)

func uniqueName(prefix string) string {
	var b [6]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// Meta returns an empty metadata store, closed when the test ends.
func Meta(t testing.TB) store.Meta {
	t.Helper()
	if dsn := os.Getenv("KUDZU_TEST_POSTGRES"); dsn != "" {
		return Postgres(t, dsn)
	}
	m, err := store.OpenSQLite(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// Postgres returns a Postgres store in a fresh schema of the database at
// dsn, dropped when the test ends.
func Postgres(t testing.TB, dsn string) *store.Postgres {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := uniqueName("kudzu_test_")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(context.Background())
	})
	m, err := store.OpenPostgres(ctx, withSearchPath(dsn, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func withSearchPath(dsn, schema string) string {
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Objects returns an empty object store.
func Objects(t testing.TB) store.Objects {
	t.Helper()
	if raw := os.Getenv("KUDZU_TEST_S3"); raw != "" {
		cfg, err := store.ParseObjectsURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Prefix += uniqueName("test-") + "/"
		s, err := store.OpenS3(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx := context.Background()
			s.Walk(ctx, func(id object.ID, _ int64, _ time.Time) error {
				_, _, err := s.DeleteIfOlder(ctx, id, time.Now().Add(time.Hour))
				return err
			})
		})
		return s
	}
	s, err := store.OpenFS(filepath.Join(t.TempDir(), "objects"), 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
