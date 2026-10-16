package client

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/sparse"
)

// Trees come from a server, a cache or a daemon, and a checkout trusts none
// of them with the names it creates. object.DecodeTree already refuses "..",
// "a/b" and unsorted or repeated names; checkDir adds what depends on the
// client's operating system.

// checkName reports why a tree entry name cannot be checked out on goos.
func checkName(goos, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return errors.New("invalid name")
	}
	if object.ReservedName(name) {
		return errors.New("reserved name")
	}
	if goos != "windows" {
		return nil // elsewhere \ and : are ordinary characters, used in real repositories
	}
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return fmt.Errorf("%q is not allowed in a file name on windows", r)
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return errors.New("windows drops a trailing dot or space")
	}
	// Device names are reserved with any extension: con.txt is the console.
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	device := false
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		device = true
	}
	if len(base) > 3 && (base[:3] == "COM" || base[:3] == "LPT") {
		r, n := utf8.DecodeRuneInString(base[3:])
		device = n == len(base)-3 && strings.ContainsRune("0123456789¹²³", r)
	}
	if device {
		return errors.New("reserved device name on windows")
	}
	return nil
}

// caseInsensitive reports whether goos's filesystems ignore case by default.
func caseInsensitive(goos string) bool { return goos == "darwin" || goos == "windows" }

// foldCase maps names that differ only in case to the same string.
func foldCase(s string) string {
	return strings.Map(func(r rune) rune {
		lo := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			lo = min(lo, f)
		}
		return lo
	}, s)
}

// materialized reports whether a checkout with spec sp creates entry e,
// found at path p. Submodules are not checked out.
func materialized(e object.Entry, p string, sp sparse.Spec) bool {
	if e.IsDir() {
		return sp.WantDir(p)
	}
	return e.Mode != object.ModeGitlink && sp.WantFile(p)
}

// checkDir vets the names a checkout creates in directory dir on goos. Where
// case is ignored, two names that differ only in case would be one file, and
// the second would overwrite the first or, if the first is a symlink, be
// written through it.
func checkDir(goos, dir string, entries []object.Entry, sp sparse.Spec) error {
	var folded map[string]string
	if caseInsensitive(goos) {
		folded = map[string]string{}
	}
	for _, e := range entries {
		p := object.JoinPath(dir, e.Name)
		if !materialized(e, p, sp) {
			continue
		}
		if err := checkName(goos, e.Name); err != nil {
			return fmt.Errorf("cannot check out %s: %w", p, err)
		}
		if folded != nil {
			k := foldCase(e.Name)
			if prev, ok := folded[k]; ok {
				return fmt.Errorf("cannot check out both %s and %s: on %s they are the same file", object.JoinPath(dir, prev), p, goos)
			}
			folded[k] = e.Name
		}
	}
	return nil
}
