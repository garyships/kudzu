// Package sparse implements "cone" sparse checkout rules shared by the
// server (snapshots) and the client (working copies): a sparse directory
// includes everything below it, plus the files directly inside each of its
// ancestors. An empty spec includes everything.
package sparse

import (
	"fmt"
	"sort"
	"strings"

	"github.com/garyships/kudzu/internal/object"
)

type Spec []string

// Normalize cleans, validates, sorts and de-duplicates directory paths.
func Normalize(dirs []string) (Spec, error) {
	var out Spec
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.Trim(strings.TrimSpace(d), "/")
		if d == "" || d == "." {
			return nil, nil // the root: everything
		}
		if err := object.ValidatePath(d); err != nil {
			return nil, fmt.Errorf("sparse path: %w", err)
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out, nil
}

func under(p, dir string) bool { return strings.HasPrefix(p, dir+"/") }

// Full reports whether the spec includes the whole repository.
func (s Spec) Full() bool { return len(s) == 0 }

// WantDir reports whether directory p must be descended into.
func (s Spec) WantDir(p string) bool {
	if len(s) == 0 || p == "" {
		return true
	}
	for _, d := range s {
		if p == d || under(p, d) || under(d, p) {
			return true
		}
	}
	return false
}

// WantFile reports whether file p is part of the checkout.
func (s Spec) WantFile(p string) bool {
	if len(s) == 0 {
		return true
	}
	parent := ""
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		parent = p[:i]
	}
	if parent == "" {
		return true
	}
	for _, d := range s {
		if under(p, d) || parent == d || under(d, parent) {
			return true
		}
	}
	return false
}
