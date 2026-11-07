package repo

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/textdiff"
)

// lowerLineLimits sets the textdiff limits for one test.
func lowerLineLimits(t *testing.T, bytes, lines int) {
	oldBytes, oldLines := textdiff.MaxBytes, textdiff.MaxLines
	textdiff.MaxBytes, textdiff.MaxLines = bytes, lines
	t.Cleanup(func() { textdiff.MaxBytes, textdiff.MaxLines = oldBytes, oldLines })
}

// TestBlameTooLarge: blame refuses a file over the line limits, and the
// blame deriver records that without splitting it into lines (a small
// stored blob of newlines used to cost hundreds of times its size). A later
// version under the limits is blamed as if new.
func TestBlameTooLarge(t *testing.T) {
	lowerLineLimits(t, 16<<10, 1000)
	r := newTestRepo(t)
	ctx := context.Background()
	c1 := commit(t, r, nil, "one", files{"f": "a\nb\n"})
	c2 := commit(t, r, []object.ID{c1}, "two", files{"f": strings.Repeat("\n", 256<<10)})
	c3 := commit(t, r, []object.ID{c2}, "three", files{"f": "a\nb\nc\n"})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := r.Blame(ctx, c2, "f")
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "too large to blame") {
		t.Fatalf("blame of a file over the limits: %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4<<20 {
		t.Fatalf("blame of a 256 KiB file allocated %d MiB", alloc>>20)
	}
	bl, err := r.Blame(ctx, c3, "f")
	if err != nil || len(bl) != 3 {
		t.Fatalf("blame after a large version: %v, %v", bl, err)
	}
	for _, l := range bl {
		if l.Changeset != c3 {
			t.Fatalf("line %d blamed on %s, want %s", l.Line, l.Changeset.Short(), c3.Short())
		}
	}
}

// TestPatchTooLarge: a patch names a file over the limits without hunks,
// and stops diffing once it has read PatchLimit times the limits (here
// 2000 bytes: 1000 for finding big too large, then small and x/1new).
func TestPatchTooLarge(t *testing.T) {
	lowerLineLimits(t, 1000, 100)
	old := PatchLimit
	PatchLimit = 2
	t.Cleanup(func() { PatchLimit = old })
	r := newTestRepo(t)
	ctx := context.Background()
	c1 := commit(t, r, nil, "one", files{"big": "x\n", "small": "s\n"})
	c2 := commit(t, r, []object.ID{c1}, "two", files{
		"big":    strings.Repeat("y\n", 101),
		"small":  "S\n",
		"x/1new": strings.Repeat("1\n", 50) + strings.Repeat("a", 600),
		"x/2new": strings.Repeat("2\n", 50) + strings.Repeat("b", 600),
		"x/3new": "3\n",
	})
	diffs, err := r.Diff(ctx, c1, c2, "")
	if err != nil {
		t.Fatal(err)
	}
	patch, err := r.Patch(ctx, diffs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"diff --kudzu a/big b/big\nFiles a/big and b/big differ (too large to diff by lines)\n",
		"-s\n+S\n",
		"+1\n",
		"Files /dev/null and b/x/2new differ (patch too large; diff this path on its own)\n",
		"Files /dev/null and b/x/3new differ (patch too large; diff this path on its own)\n",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}
	if strings.Contains(patch, "+y\n") {
		t.Errorf("a file over the limits was diffed by lines:\n%s", patch)
	}
}

// TestLineWorkWaitsForSlot: blame computations and merges of files wait for
// a line-work slot, and give up when their caller does.
func TestLineWorkWaitsForSlot(t *testing.T) {
	r := newTestRepo(t)
	base := commit(t, r, nil, "base", files{"f": "a\nb\nc\nd\ne\n"})
	upstream := commit(t, r, []object.ID{base}, "upstream", files{"f": "A\nb\nc\nd\ne\n"})
	mine := commit(t, r, []object.ID{base}, "mine", files{"f": "a\nb\nc\nd\nE\n"})
	r.lineWork = make(chan struct{}, 1)
	r.lineWork <- struct{}{} // all slots busy
	for what, work := range map[string]func(context.Context) error{
		"blame": func(ctx context.Context) error {
			_, err := r.Blame(ctx, mine, "f")
			return err
		},
		"rebase": func(ctx context.Context) error {
			_, err := r.Rebase(ctx, mine, upstream)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := work(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s with every slot busy: %v", what, err)
		}
	}
	<-r.lineWork
	if _, err := r.Blame(context.Background(), mine, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rebase(context.Background(), mine, upstream); err != nil {
		t.Fatal(err)
	}
}

// TestMergeTooLarge: a file over the limits changed on both sides is a
// conflict rather than a line-based merge, unless one side left it alone.
func TestMergeTooLarge(t *testing.T) {
	r := newTestRepo(t)
	lines := strings.Repeat("line\n", 40)
	base := commit(t, r, nil, "base", files{"f": lines, "g": "g\n"})
	upstream := commit(t, r, []object.ID{base}, "upstream", files{"f": "top\n" + lines})
	mine := commit(t, r, []object.ID{base}, "mine", files{"f": lines + "bottom\n"})
	other := commit(t, r, []object.ID{base}, "other", files{"g": "G\n"})
	ctx := context.Background()
	if _, err := r.Rebase(ctx, mine, upstream); err != nil {
		t.Fatalf("edits far apart should merge: %v", err)
	}
	lowerLineLimits(t, 1<<20, 40)
	_, err := r.Rebase(ctx, mine, upstream)
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Paths) != 1 || ce.Paths[0] != "f" {
		t.Fatalf("rebase of edits to a file over the limits: %v", err)
	}
	if _, err := r.Rebase(ctx, other, upstream); err != nil {
		t.Fatalf("rebase leaving the large file alone: %v", err)
	}
}
