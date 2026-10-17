package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/garyships/kudzu/internal/object"
)

// A bounded cache's size covers the objects already on disk when it is
// opened, however late it is first needed.
func TestFSSizeCountsExistingObjects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	put := func(s *FS, i int) int64 {
		t.Helper()
		id, stored := object.EncodeObject(object.KindBlob, []byte(fmt.Sprintf("object %d\n", i)))
		if err := s.Put(ctx, id, stored); err != nil {
			t.Fatal(err)
		}
		return int64(len(stored))
	}
	s, err := OpenFS(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for i := 0; i < 10; i++ {
		total += put(s, i)
	}
	if s.Size() != total {
		t.Fatalf("size = %d, want %d", s.Size(), total)
	}
	s, err = OpenFS(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	total += put(s, 10)
	if s.Size() != total {
		t.Fatalf("after reopening and writing: size = %d, want %d", s.Size(), total)
	}
	s, err = OpenFS(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size() != total {
		t.Fatalf("after reopening: size = %d, want %d", s.Size(), total)
	}
}

// BenchmarkOpenBoundedFS opens a bounded cache holding 50k objects, as every
// command run inside a working copy does with .kudzu/objects.
func BenchmarkOpenBoundedFS(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	const max = 1 << 40
	s, err := OpenFS(dir, max)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50_000; i++ {
		id, stored := object.EncodeObject(object.KindBlob, []byte(fmt.Sprintf("object %d\n", i)))
		if err := s.Put(ctx, id, stored); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("open", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := OpenFS(dir, max); err != nil {
				b.Fatal(err)
			}
		}
	})
	// The first write after opening still has to size the cache.
	n := 0
	b.Run("open+put", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s, err := OpenFS(dir, max)
			if err != nil {
				b.Fatal(err)
			}
			n++
			id, stored := object.EncodeObject(object.KindBlob, []byte(fmt.Sprintf("new object %d\n", n)))
			if err := s.Put(ctx, id, stored); err != nil {
				b.Fatal(err)
			}
		}
	})
}
