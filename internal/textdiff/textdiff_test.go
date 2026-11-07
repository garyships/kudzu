package textdiff

import (
	"math/rand"
	"strings"
	"testing"
)

// apply reconstructs b from a and the hunks, proving the diff is complete.
func apply(a, b []string, hs []Hunk) []string {
	var out []string
	pos := 0
	for _, h := range hs {
		out = append(out, a[pos:h.A0]...)
		out = append(out, b[h.B0:h.B1]...)
		pos = h.A1
	}
	return append(out, a[pos:]...)
}

func randLines(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = string(rune('a'+r.Intn(5))) + "\n"
	}
	return out
}

func mutate(r *rand.Rand, a []string) []string {
	b := append([]string(nil), a...)
	for k := r.Intn(6); k > 0; k-- {
		i := r.Intn(len(b) + 1)
		switch r.Intn(3) {
		case 0:
			b = append(b[:i], append([]string{"new\n"}, b[i:]...)...)
		case 1:
			if i < len(b) {
				b = append(b[:i], b[i+1:]...)
			}
		case 2:
			if i < len(b) {
				b[i] = "changed\n"
			}
		}
	}
	return b
}

func TestLinesRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		a := randLines(r, r.Intn(30))
		b := mutate(r, a)
		if r.Intn(4) == 0 {
			b = randLines(r, r.Intn(30))
		}
		hs := Lines(a, b)
		got := apply(a, b, hs)
		if strings.Join(got, "") != strings.Join(b, "") {
			t.Fatalf("iter %d: reconstruct mismatch\na=%q\nb=%q\nhunks=%v", iter, a, b, hs)
		}
		// Hunks must be minimal-ish: equal regions really are equal.
		pos, posB := 0, 0
		for _, h := range hs {
			if h.A0-pos != h.B0-posB {
				t.Fatalf("misaligned hunks %v", hs)
			}
			for k := 0; k < h.A0-pos; k++ {
				if a[pos+k] != b[posB+k] {
					t.Fatalf("unequal context")
				}
			}
			pos, posB = h.A1, h.B1
		}
	}
}

func TestMerge3(t *testing.T) {
	base := "a\nb\nc\nd\ne\nf\n"
	ours := "a\nB\nc\nd\ne\nf\n"
	theirs := "a\nb\nc\nd\nE\nf\n"
	got, ok := Merge3([]byte(base), []byte(ours), []byte(theirs))
	if !ok || string(got) != "a\nB\nc\nd\nE\nf\n" {
		t.Fatalf("clean merge failed: ok=%v got=%q", ok, got)
	}
	_, ok = Merge3([]byte(base), []byte("a\nX\nc\nd\ne\nf\n"), []byte("a\nY\nc\nd\ne\nf\n"))
	if ok {
		t.Fatal("expected conflict")
	}
	got, ok = Merge3([]byte(base), []byte("a\nX\nc\nd\ne\nf\n"), []byte("a\nX\nc\nd\ne\nf\n"))
	if !ok || string(got) != "a\nX\nc\nd\ne\nf\n" {
		t.Fatal("identical change should merge")
	}
	got, ok = Merge3([]byte(base), []byte("top\n"+base), []byte(base+"bottom\n"))
	if !ok || string(got) != "top\n"+base+"bottom\n" {
		t.Fatalf("insertions at both ends: %q", got)
	}
}

func TestMerge3Random(t *testing.T) {
	// Disjoint edits to far-apart regions must always merge cleanly and
	// contain both edits.
	r := rand.New(rand.NewSource(7))
	for iter := 0; iter < 500; iter++ {
		var base []string
		for i := 0; i < 40; i++ {
			base = append(base, "line"+string(rune('A'+i%26))+string(rune('a'+i/26))+"\n")
		}
		ours := append([]string(nil), base...)
		theirs := append([]string(nil), base...)
		i, j := r.Intn(10), 20+r.Intn(10)
		ours[i] = "ours\n"
		theirs[j] = "theirs\n"
		got, ok := Merge3([]byte(strings.Join(base, "")), []byte(strings.Join(ours, "")), []byte(strings.Join(theirs, "")))
		want := append([]string(nil), base...)
		want[i], want[j] = "ours\n", "theirs\n"
		if !ok || string(got) != strings.Join(want, "") {
			t.Fatalf("iter %d: ok=%v", iter, ok)
		}
	}
}

func TestUnified(t *testing.T) {
	out := Unified("a/x", "b/x", []byte("1\n2\n3\n"), []byte("1\nTWO\n3\n"), 3)
	want := "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n 1\n-2\n+TWO\n 3\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if Unified("a", "b", []byte("x"), []byte("x"), 3) != "" {
		t.Fatal("equal inputs should produce no diff")
	}
}

// lowerLimits sets MaxBytes and MaxLines for one test.
func lowerLimits(t *testing.T, bytes, lines int) {
	oldBytes, oldLines := MaxBytes, MaxLines
	MaxBytes, MaxLines = bytes, lines
	t.Cleanup(func() { MaxBytes, MaxLines = oldBytes, oldLines })
}

// TestTooLarge: content over either limit is not diffed or merged by lines.
func TestTooLarge(t *testing.T) {
	lowerLimits(t, 100, 10)
	small := strings.Repeat("x\n", 10)
	longLines := strings.Repeat("y", 101)
	manyLines := strings.Repeat("\n", 10) + "z"
	for _, c := range []struct {
		s    string
		want bool
	}{{small, false}, {longLines, true}, {manyLines, true}} {
		if got := TooLarge([]byte(c.s)); got != c.want {
			t.Errorf("TooLarge(%q) = %v", c.s, got)
		}
	}
	for _, big := range []string{longLines, manyLines} {
		if got := Unified("a/f", "b/f", []byte(small), []byte(big), 3); got != "Files a/f and b/f differ (too large to diff by lines)\n" {
			t.Errorf("Unified of large content = %q", got)
		}
	}
	// Edits far apart merge, but not when a version is too large.
	base := strings.Repeat("l\n", 10)
	ours := "O\n" + base[2:]
	theirs := base[:18] + "T\n"
	if _, ok := Merge3([]byte(base), []byte(ours), []byte(theirs)); !ok {
		t.Fatal("small disjoint edits should merge")
	}
	if _, ok := Merge3([]byte(base), []byte(ours), []byte(theirs+"more\n")); ok {
		t.Fatal("a version over MaxLines was merged by lines")
	}
	// A change on one side only needs no line-based work.
	if got, ok := Merge3([]byte(manyLines), []byte(manyLines), []byte(small)); !ok || string(got) != small {
		t.Fatalf("one-sided change of large content: ok=%v", ok)
	}
}

// TestSplitLinesSharesContent: the lines are slices of one copy of the
// content, not one allocation each.
func TestSplitLinesSharesContent(t *testing.T) {
	b := []byte(strings.Repeat("line\n", 1000) + "last")
	lines := SplitLines(b)
	if len(lines) != 1001 || strings.Join(lines, "") != string(b) {
		t.Fatalf("got %d lines", len(lines))
	}
	if n := testing.AllocsPerRun(10, func() { SplitLines(b) }); n > 2 {
		t.Fatalf("SplitLines made %v allocations for 1001 lines", n)
	}
	if SplitLines(nil) != nil {
		t.Fatal("no content should give no lines")
	}
}
