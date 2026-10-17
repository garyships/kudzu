package sparse

import (
	"fmt"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want Spec
	}{
		{nil, nil},
		{[]string{"b/", "/a", "a", " c/d "}, Spec{"a", "b", "c/d"}},
		{[]string{"a", "."}, nil}, // the root means everything
		{[]string{"a", ""}, nil},
	} {
		got, err := Normalize(c.in)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("Normalize(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"a/../b", ".kudzu", "a//b"} {
		if _, err := Normalize([]string{bad}); err == nil {
			t.Errorf("Normalize(%q) should fail", bad)
		}
	}
}

// TestCone checks git's cone rules: everything under a chosen directory, the
// directories leading to it, and the files directly inside each of those.
func TestCone(t *testing.T) {
	sp, _ := Normalize([]string{"services/api"})
	for dir, want := range map[string]bool{
		"":                  true,
		"services":          true,
		"services/api":      true,
		"services/api/v1":   true,
		"services/web":      false,
		"services/apiextra": false,
		"docs":              false,
	} {
		if got := sp.WantDir(dir); got != want {
			t.Errorf("WantDir(%q) = %v, want %v", dir, got, want)
		}
	}
	for file, want := range map[string]bool{
		"README.md":                true, // root files are always in
		"services/BUILD":           true, // files directly in an ancestor
		"services/api/main.go":     true,
		"services/api/v1/h.go":     true,
		"services/web/app.js":      false,
		"services/apiextra/x.go":   false,
		"docs/guide.md":            false,
		"services/web/deep/x.html": false,
	} {
		if got := sp.WantFile(file); got != want {
			t.Errorf("WantFile(%q) = %v, want %v", file, got, want)
		}
	}
	var full Spec
	if !full.Full() || !full.WantDir("anything") || !full.WantFile("any/file") {
		t.Error("an empty spec includes everything")
	}
}
