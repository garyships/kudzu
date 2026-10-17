package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/garyships/kudzu/internal/object"
)

// testWC is a working copy laid out on disk without a server: track writes
// a file and records it in the index as checked out.
type testWC struct {
	*WC
	tb testing.TB
}

func newTestWC(tb testing.TB, ignore string) testWC {
	tb.Helper()
	root := tb.TempDir()
	if err := os.MkdirAll(filepath.Join(root, metaDir), 0o755); err != nil {
		tb.Fatal(err)
	}
	w := testWC{&WC{Root: root, Index: map[string]IndexEntry{}}, tb}
	w.track(".kudzuignore", ignore)
	return w
}

func (w testWC) write(p, content string) {
	w.tb.Helper()
	full := w.abs(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		w.tb.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		w.tb.Fatal(err)
	}
}

func (w testWC) track(p, content string) {
	w.tb.Helper()
	w.write(p, content)
	info, err := os.Lstat(w.abs(p))
	if err != nil {
		w.tb.Fatal(err)
	}
	w.Index[p] = IndexEntry{Mode: object.ModeFile, Blob: object.Hash(object.KindBlob, []byte(content)), Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
}

// An ignored directory is skipped unless it holds tracked files, which
// status still checks.
func TestStatusIgnoredDirs(t *testing.T) {
	w := newTestWC(t, "__pycache__/\nbuild/\n")
	w.track("a/mod.py", "x = 1\n")
	w.write("a/__pycache__/mod.pyc", "junk")
	w.track("build/keep.txt", "keep\n")
	w.write("build/keep.txt", "changed\n")
	w.track("x/y/build/z/deep.txt", "deep\n")
	w.write("x/y/build/z/deep.txt", "deeper\n")
	w.track("buildx/f.txt", "f\n")
	w.write("x/build/out.o", "junk")
	w.write("new.txt", "new\n")
	st, err := w.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(st)
	want := fmt.Sprint([]FileStatus{
		{Path: "build/keep.txt", Code: 'M', Mode: object.ModeFile},
		{Path: "new.txt", Code: '?', Mode: object.ModeFile},
		{Path: "x/y/build/z/deep.txt", Code: 'M', Mode: object.ModeFile},
	})
	if got != want {
		t.Fatalf("status = %s\nwant     %s", got, want)
	}
}

// BenchmarkStatusIgnoredDirs runs status on a Python-style tree: 2,000
// packages, each with an ignored __pycache__ directory, and a 20,000-file
// index.
func BenchmarkStatusIgnoredDirs(b *testing.B) {
	w := newTestWC(b, "__pycache__/\n")
	for i := 0; i < 2000; i++ {
		pkg := fmt.Sprintf("pkg%04d", i)
		w.track(pkg+"/mod.py", "x = 1\n")
		w.write(pkg+"/__pycache__/mod.cpython-312.pyc", "junk")
	}
	for i := 0; i < 18000; i++ {
		w.track(fmt.Sprintf("lib/d%02d/f%05d.txt", i%100, i), "data\n")
	}
	b.Run("status", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			st, err := w.Status(context.Background())
			if err != nil || len(st) != 0 {
				b.Fatalf("status = %v, %v", st, err)
			}
		}
	})
}
