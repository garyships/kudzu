// Package textdiff implements line diffs (Myers), three-way merge and unified
// diff output. Blame, server-side rebase/merge and the diff API use it.
package textdiff

import (
	"bytes"
	"fmt"
	"strings"
)

// SplitLines splits b into lines, keeping each line's trailing newline so the
// lines concatenate back to b exactly.
func SplitLines(b []byte) []string {
	var out []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, string(b))
			break
		}
		out = append(out, string(b[:i+1]))
		b = b[i+1:]
	}
	return out
}

func IsBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// Hunk is a differing region: a[A0:A1] was replaced by b[B0:B1].
type Hunk struct{ A0, A1, B0, B1 int }

// maxEditDistance bounds Myers' O(D^2) trace memory. Past it, the differing
// middle is reported as a single replaced region.
const maxEditDistance = 2000

type interner map[string]int

func (in interner) ids(lines []string) []int {
	out := make([]int, len(lines))
	for i, l := range lines {
		id, ok := in[l]
		if !ok {
			id = len(in)
			in[l] = id
		}
		out[i] = id
	}
	return out
}

// Lines returns the hunks turning a into b, in order.
func Lines(a, b []string) []Hunk {
	in := interner{}
	return diffInts(in.ids(a), in.ids(b))
}

func diffInts(a, b []int) []Hunk {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(am) == 0 && len(bm) == 0 {
		return nil
	}
	var hunks []Hunk
	x, y := 0, 0
	for _, r := range myers(am, bm) {
		if r.x > x || r.y > y {
			hunks = append(hunks, Hunk{pre + x, pre + r.x, pre + y, pre + r.y})
		}
		x, y = r.x+r.n, r.y+r.n
	}
	if x < len(am) || y < len(bm) {
		hunks = append(hunks, Hunk{pre + x, pre + len(am), pre + y, pre + len(bm)})
	}
	return hunks
}

// run is a diagonal of n equal lines starting at a[x], b[y].
type run struct{ x, y, n int }

func myers(a, b []int) []run {
	n, m := len(a), len(b)
	limit := n + m
	if limit > maxEditDistance {
		limit = maxEditDistance
	}
	off := limit + 1
	v := make([]int32, 2*off+1)
	var trace [][]int32
	for d := 0; d <= limit; d++ {
		snap := make([]int32, 2*d+1)
		copy(snap, v[off-d:off+d+1])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1])
			} else {
				x = int(v[off+k-1]) + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				return backtrack(trace, n, m, d)
			}
		}
	}
	return nil
}

func backtrack(trace [][]int32, n, m, dEnd int) []run {
	x, y := n, m
	var runs []run
	for d := dEnd; d > 0; d-- {
		vp := trace[d] // state before step d, index k+d
		k := x - y
		var pk int
		if k == -d || (k != d && vp[k-1+d] < vp[k+1+d]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := int(vp[pk+d])
		py := px - pk
		sx, sy := px+1, py
		if pk == k+1 {
			sx, sy = px, py+1
		}
		if x > sx {
			runs = append(runs, run{sx, sy, x - sx})
		}
		x, y = px, py
	}
	if x > 0 {
		runs = append(runs, run{0, 0, x})
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs
}

// Merge3 performs a line-based three-way merge. ok is false when the two
// sides changed overlapping or adjacent regions differently.
func Merge3(base, ours, theirs []byte) (merged []byte, ok bool) {
	switch {
	case bytes.Equal(ours, theirs), bytes.Equal(base, theirs):
		return ours, true
	case bytes.Equal(base, ours):
		return theirs, true
	}
	bl, ol, tl := SplitLines(base), SplitLines(ours), SplitLines(theirs)
	in := interner{}
	bi, oi, ti := in.ids(bl), in.ids(ol), in.ids(tl)
	type sided struct {
		h    Hunk
		ours bool
	}
	var all []sided
	ho, ht := diffInts(bi, oi), diffInts(bi, ti)
	for i, j := 0, 0; i < len(ho) || j < len(ht); {
		if j >= len(ht) || (i < len(ho) && ho[i].A0 <= ht[j].A0) {
			all = append(all, sided{ho[i], true})
			i++
		} else {
			all = append(all, sided{ht[j], false})
			j++
		}
	}
	var out []string
	pos := 0
	for i := 0; i < len(all); {
		gs, ge := all[i].h.A0, all[i].h.A1
		j := i + 1
		for j < len(all) && all[j].h.A0 <= ge {
			if all[j].h.A1 > ge {
				ge = all[j].h.A1
			}
			j++
		}
		var oh, th []Hunk
		for _, s := range all[i:j] {
			if s.ours {
				oh = append(oh, s.h)
			} else {
				th = append(th, s.h)
			}
		}
		out = append(out, bl[pos:gs]...)
		switch {
		case len(th) == 0:
			out = append(out, sideRange(ol, oh, gs, ge)...)
		case len(oh) == 0:
			out = append(out, sideRange(tl, th, gs, ge)...)
		default:
			o, t := sideRange(ol, oh, gs, ge), sideRange(tl, th, gs, ge)
			if strings.Join(o, "") != strings.Join(t, "") {
				return nil, false
			}
			out = append(out, o...)
		}
		pos = ge
		i = j
	}
	out = append(out, bl[pos:]...)
	return []byte(strings.Join(out, "")), true
}

// sideRange returns the lines of one side corresponding to base[gs:ge], given
// that side's hunks inside the group. Outside its hunks a side equals base.
func sideRange(lines []string, hs []Hunk, gs, ge int) []string {
	first, last := hs[0], hs[len(hs)-1]
	return lines[first.B0-(first.A0-gs) : last.B1+(ge-last.A1)]
}

// Unified renders a unified diff with the given number of context lines.
// It returns "" when a and b are equal.
func Unified(aName, bName string, a, b []byte, context int) string {
	if bytes.Equal(a, b) {
		return ""
	}
	if IsBinary(a) || IsBinary(b) {
		return fmt.Sprintf("Binary files %s and %s differ\n", aName, bName)
	}
	al, bl := SplitLines(a), SplitLines(b)
	hs := Lines(al, bl)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", aName, bName)
	writeLine := func(prefix byte, l string) {
		sb.WriteByte(prefix)
		sb.WriteString(l)
		if !strings.HasSuffix(l, "\n") {
			sb.WriteString("\n\\ No newline at end of file\n")
		}
	}
	for i := 0; i < len(hs); {
		j := i
		for j+1 < len(hs) && hs[j+1].A0-hs[j].A1 <= 2*context {
			j++
		}
		a0 := max(hs[i].A0-context, 0)
		a1 := min(hs[j].A1+context, len(al))
		b0 := hs[i].B0 - (hs[i].A0 - a0)
		b1 := hs[j].B1 + (a1 - hs[j].A1)
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", hunkRange(a0, a1-a0), hunkRange(b0, b1-b0))
		pos := a0
		for k := i; k <= j; k++ {
			h := hs[k]
			for ; pos < h.A0; pos++ {
				writeLine(' ', al[pos])
			}
			for _, l := range al[h.A0:h.A1] {
				writeLine('-', l)
			}
			for _, l := range bl[h.B0:h.B1] {
				writeLine('+', l)
			}
			pos = h.A1
		}
		for ; pos < a1; pos++ {
			writeLine(' ', al[pos])
		}
		i = j + 1
	}
	return sb.String()
}

func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}
