package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
)

type gcObjects interface {
	Objects
	Toucher
	Sweeper
}

// testObjects checks the behavior every object store must share. age moves
// the store's clock forward (nil if it cannot).
func testObjects(t *testing.T, s gcObjects, age func(time.Duration)) {
	ctx := context.Background()
	var ids []object.ID
	for i := 0; i < 8; i++ {
		id, stored := object.EncodeObject(object.KindBlob, []byte(fmt.Sprintf("object %d\n", i)))
		if err := s.Put(ctx, id, stored); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := s.Put(ctx, ids[0], object.Encode(object.KindBlob, []byte("object 0\n"))); err != nil {
		t.Fatalf("re-putting an object: %v", err)
	}
	b, err := s.Get(ctx, ids[3])
	if err != nil {
		t.Fatal(err)
	}
	if _, content, err := object.Verify(ids[3], b); err != nil || string(content) != "object 3\n" {
		t.Fatalf("get = %q, %v", content, err)
	}
	missing := object.Hash(object.KindBlob, []byte("never stored"))
	if _, err := s.Get(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of a missing object = %v", err)
	}
	if ok, err := s.Has(ctx, missing); ok || err != nil {
		t.Fatalf("has of a missing object = %v, %v", ok, err)
	}
	if ok, err := s.Has(ctx, ids[1]); !ok || err != nil {
		t.Fatalf("has = %v, %v", ok, err)
	}
	if err := s.Touch(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch of a missing object = %v", err)
	}
	var walked []string
	err = s.Walk(ctx, func(id object.ID, size int64, mtime time.Time) error {
		walked = append(walked, id.String())
		if size <= 0 || mtime.IsZero() {
			t.Errorf("walk: %s size %d mtime %v", id.Short(), size, mtime)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, id := range ids {
		want = append(want, id.String())
	}
	sort.Strings(want)
	sort.Strings(walked)
	if fmt.Sprint(walked) != fmt.Sprint(want) {
		t.Fatalf("walk found %d objects, want %d", len(walked), len(want))
	}

	if removed, _, err := s.DeleteIfOlder(ctx, ids[0], time.Now().Add(-time.Hour)); removed || err != nil {
		t.Fatalf("deleted an object newer than the cutoff: %v", err)
	}
	if age != nil {
		age(time.Hour)
		if err := s.Touch(ctx, ids[1]); err != nil {
			t.Fatal(err)
		}
		cutoff := time.Now().Add(-30 * time.Minute)
		if removed, _, err := s.DeleteIfOlder(ctx, ids[1], cutoff); removed || err != nil {
			t.Fatalf("deleted a touched object: %v", err)
		}
		if removed, _, err := s.DeleteIfOlder(ctx, ids[2], cutoff); !removed || err != nil {
			t.Fatalf("kept an old object: %v", err)
		}
	}
	removed, size, err := s.DeleteIfOlder(ctx, ids[0], time.Now().Add(24*time.Hour))
	if !removed || size == 0 || err != nil {
		t.Fatalf("delete = %v %d %v", removed, size, err)
	}
	if ok, _ := s.Has(ctx, ids[0]); ok {
		t.Fatal("deleted object is still there")
	}
	if removed, _, err := s.DeleteIfOlder(ctx, ids[0], time.Now().Add(24*time.Hour)); removed || err != nil {
		t.Fatalf("deleting twice = %v, %v", removed, err)
	}
}

func TestFSObjects(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFS(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	testObjects(t, s, func(d time.Duration) {
		filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				old := time.Now().Add(-d)
				os.Chtimes(p, old, old)
			}
			return nil
		})
	})
}

func TestS3ObjectsFake(t *testing.T) {
	f, cfg := newFakeS3(t)
	s, err := OpenS3(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	testObjects(t, s, func(d time.Duration) {
		f.mu.Lock()
		for k, o := range f.objs {
			o.mod = o.mod.Add(-d)
			f.objs[k] = o
		}
		f.mu.Unlock()
	})
	if f.requests["PUT"] == 0 || f.requests["HEAD"] == 0 {
		t.Fatalf("requests = %v", f.requests)
	}
	if _, err := OpenS3(context.Background(), S3Config{Endpoint: cfg.Endpoint, Bucket: "other", Region: "us-east-1", PathStyle: true, AccessKey: "AK", SecretKey: "SK"}); err == nil {
		t.Fatal("opening a missing bucket should fail")
	}
}

// TestS3ObjectsLive runs against a real S3-compatible service when
// KUDZU_TEST_S3 names a bucket (for example
// s3://kudzu-test?endpoint=http://127.0.0.1:9000 for MinIO), with
// credentials in AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
func TestS3ObjectsLive(t *testing.T) {
	raw := os.Getenv("KUDZU_TEST_S3")
	if raw == "" {
		t.Skip("KUDZU_TEST_S3 not set")
	}
	cfg, err := ParseObjectsURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Prefix += fmt.Sprintf("conformance-%d/", time.Now().UnixNano())
	s, err := OpenS3(context.Background(), cfg)
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
	testObjects(t, s, nil)

	// Touch refreshes an old object with a copy onto itself, which S3 only
	// allows when the copy replaces the metadata.
	id, stored := object.EncodeObject(object.KindBlob, []byte("touch me\n"))
	if err := s.Put(context.Background(), id, stored); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := s.Touch(context.Background(), id); err != nil {
		t.Fatalf("touch by copy: %v", err)
	}
	if b, err := s.Get(context.Background(), id); err != nil || string(b) != string(stored) {
		t.Fatalf("object after touch = %q, %v", b, err)
	}
}

func TestParseObjectsURL(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AK")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")
	t.Setenv("AWS_REGION", "eu-west-1")
	for raw, want := range map[string]S3Config{
		"s3://bkt":                                   {Bucket: "bkt", Region: "eu-west-1"},
		"s3://bkt/a/b/?region=us-west-2":             {Bucket: "bkt", Prefix: "a/b/", Region: "us-west-2"},
		"s3://bkt/p?endpoint=http://127.0.0.1:9000/": {Bucket: "bkt", Prefix: "p/", Region: "eu-west-1", Endpoint: "http://127.0.0.1:9000", PathStyle: true},
		"s3://bkt?endpoint=https://x.r2.dev&region=auto&path_style=false": {Bucket: "bkt", Region: "auto", Endpoint: "https://x.r2.dev"},
		"gs://bkt/kudzu": {Bucket: "bkt", Prefix: "kudzu/", Region: "auto", Endpoint: "https://storage.googleapis.com", PathStyle: true},
	} {
		got, err := ParseObjectsURL(raw)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		want.AccessKey, want.SecretKey = "AK", "SK"
		if got != want {
			t.Errorf("%s:\n got  %+v\n want %+v", raw, got, want)
		}
	}
	for _, raw := range []string{"s3://", "ftp://bkt", "s3://bkt?path_style=maybe"} {
		if _, err := ParseObjectsURL(raw); err == nil {
			t.Errorf("%s should fail", raw)
		}
	}
}

// testMeta checks the behavior every Meta must share.
func testMeta(t *testing.T, m Meta) {
	ctx := context.Background()
	ids := make([]object.ID, 3)
	var seqs []int64
	for i := range ids {
		raw := []byte(fmt.Sprintf(`{"n":%d}`, i))
		ids[i] = object.Hash(object.KindChangeset, raw)
		seq, err := m.AppendChangeset(ctx, ids[i], raw)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	if !(seqs[0] < seqs[1] && seqs[1] < seqs[2]) {
		t.Fatalf("log positions not increasing: %v", seqs)
	}
	if seq, err := m.AppendChangeset(ctx, ids[1], []byte(`{"n":1}`)); err != nil || seq != seqs[1] {
		t.Fatalf("re-append = %d, %v; want %d", seq, err, seqs[1])
	}
	raw, seq, err := m.GetChangeset(ctx, ids[2])
	if err != nil || seq != seqs[2] || string(raw) != `{"n":2}` {
		t.Fatalf("get = %s %d %v", raw, seq, err)
	}
	if _, _, err := m.GetChangeset(ctx, object.Zero); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	if got, err := m.ResolvePrefix(ctx, ids[0].String()[:12]); err != nil || got != ids[0] {
		t.Fatalf("resolve prefix = %v %v", got, err)
	}
	if _, err := m.ResolvePrefix(ctx, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve bad prefix = %v", err)
	}
	wal, err := m.WAL(ctx, seqs[0], 10)
	if err != nil || len(wal) != 2 || wal[0].ID != ids[1] || wal[1].Seq != seqs[2] {
		t.Fatalf("wal = %+v %v", wal, err)
	}
	if now := time.Now().Unix(); wal[0].Created < now-60 || wal[0].Created > now+60 {
		t.Fatalf("created = %d", wal[0].Created)
	}
	if n, err := m.DeleteChangesets(ctx, ids[:1], time.Now().Unix()-60); n != 0 || err != nil {
		t.Fatalf("deleted %d recent changesets (%v)", n, err)
	}
	if n, err := m.DeleteChangesets(ctx, ids[:1], time.Now().Unix()+60); n != 1 || err != nil {
		t.Fatalf("delete = %d %v", n, err)
	}
	if wal, _ := m.WAL(ctx, 0, 10); len(wal) != 2 || wal[0].Seq != seqs[1] {
		t.Fatalf("wal after delete = %+v", wal)
	}

	if err := m.TouchChangeset(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := m.TouchChangeset(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touching a deleted changeset = %v", err)
	}
	if err := m.CASRef(ctx, "main", object.Zero, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pointing a ref at a changeset not in the log = %v", err)
	}
	if err := m.CASRef(ctx, "main", object.Zero, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := m.CASRef(ctx, "main", object.Zero, ids[2]); !errors.Is(err, ErrConflict) {
		t.Fatalf("creating an existing ref = %v", err)
	}
	if err := m.CASRef(ctx, "main", ids[2], ids[1]); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS from the wrong value = %v", err)
	}
	if err := m.CASRef(ctx, "main", ids[1], ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := m.CASRef(ctx, "topic", object.Zero, ids[1]); err != nil {
		t.Fatal(err)
	}
	if refs, err := m.ListRefs(ctx); err != nil || len(refs) != 2 || refs["main"] != ids[2] {
		t.Fatalf("refs = %v %v", refs, err)
	}
	if err := m.CASRef(ctx, "topic", ids[1], object.Zero); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetRef(ctx, "topic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted ref = %v", err)
	}

	if err := m.PutDerived(ctx, "tree", ids[1], []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := m.PutDerived(ctx, "tree", ids[1], []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if v, err := m.GetDerived(ctx, "tree", ids[1]); err != nil || string(v) != "v1" {
		t.Fatalf("derived = %s %v (the first value wins)", v, err)
	}
	if n, err := m.DeleteDerived(ctx, "tree", ids); n != 1 || err != nil {
		t.Fatalf("delete derived = %d %v", n, err)
	}
	if _, err := m.GetDerived(ctx, "tree", ids[1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted derived = %v", err)
	}

	m.PutMapping(ctx, "git", "a", "1")
	m.PutMapping(ctx, "git", "a", "2")
	m.PutMapping(ctx, "other", "b", "3")
	if v, err := m.GetMapping(ctx, "git", "a"); err != nil || v != "2" {
		t.Fatalf("mapping = %s %v", v, err)
	}
	var all []string
	m.Mappings(ctx, "", func(ns, k, v string) error { all = append(all, ns+":"+k+"="+v); return nil })
	if fmt.Sprint(all) != "[git:a=2 other:b=3]" {
		t.Fatalf("mappings = %v", all)
	}
	if err := m.DeleteMapping(ctx, "git", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetMapping(ctx, "git", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted mapping = %v", err)
	}
}

func TestSQLiteMeta(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testMeta(t, s)
}

// TestMeta lets the external test package run testMeta (storetest imports
// store, so tests that need it live in package store_test).
var TestMeta = testMeta
