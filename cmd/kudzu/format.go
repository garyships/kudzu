package main

import (
	"fmt"
	"strings"
)

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// cellWidth approximates how many terminal columns r occupies: East Asian
// wide and fullwidth characters (CJK, Hangul, fullwidth forms, emoji) take two.
func cellWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

// fitWidth truncates s to at most w terminal columns and pads it to exactly w.
func fitWidth(s string, w int) string {
	var b strings.Builder
	used := 0
	for _, r := range s {
		cw := cellWidth(r)
		if used+cw > w {
			break
		}
		b.WriteRune(r)
		used += cw
	}
	b.WriteString(strings.Repeat(" ", w-used))
	return b.String()
}
