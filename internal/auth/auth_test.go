package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func policy(t *testing.T, body string) *Policy {
	t.Helper()
	p, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRules(t *testing.T) {
	p := policy(t, fmt.Sprintf(`{"principals": {"alice": {
		"token_sha256": [%q],
		"read":  ["", "secrets/public"],
		"write": ["services/api", "secrets/public/drafts"],
		"deny":  ["secrets", "services/api/keys"],
		"land":  ["main", "users/alice/*"]
	}}}`, hashOf("t1")))
	a, err := p.Authenticate("t1")
	if err != nil || a.Name != "alice" {
		t.Fatalf("authenticate: %v %v", a, err)
	}
	for path, want := range map[string]Level{
		"":                           Read,
		"README.md":                  Read,
		"services":                   Read,
		"services/api":               Write,
		"services/api/main.go":       Write,
		"services/apix/main.go":      Read, // prefixes match whole components
		"services/api/keys":          None,
		"services/api/keys/k.pem":    None,
		"secrets":                    None,
		"secrets/db.txt":             None,
		"secrets/public/notes.md":    Read,
		"secrets/public/drafts/x.md": Write,
	} {
		if got := a.Level(path); got != want {
			t.Errorf("Level(%q) = %s, want %s", path, got, want)
		}
	}
	for dir, want := range map[string]bool{"": true, "secrets": true, "secrets/other": false, "services/api/keys": false} {
		if got := a.CanSee(dir); got != want {
			t.Errorf("CanSee(%q) = %v, want %v", dir, got, want)
		}
	}
	for dir, want := range map[string]bool{"": false, "docs": true, "services/api": false, "secrets/public": true, "secrets": false} {
		if got := a.ReadsAll(dir); got != want {
			t.Errorf("ReadsAll(%q) = %v, want %v", dir, got, want)
		}
	}
	for ref, want := range map[string]bool{"main": true, "main2": false, "users/alice/x": true, "users/bob/x": false} {
		if got := a.CanLand(ref); got != want {
			t.Errorf("CanLand(%q) = %v, want %v", ref, got, want)
		}
	}
	var de *DeniedError
	if err := a.CheckWrite("README.md"); !errors.As(err, &de) || de.Path != "README.md" || !strings.Contains(err.Error(), "alice may not write README.md") {
		t.Errorf("CheckWrite = %v", err)
	}
	if err := a.CheckLand("prod"); err == nil || !strings.Contains(err.Error(), "may not move ref prod") {
		t.Errorf("CheckLand = %v", err)
	}
	if !a.WritesAnywhere() {
		t.Error("alice writes somewhere")
	}
}

func TestEqualPrefixPrecedence(t *testing.T) {
	p := policy(t, fmt.Sprintf(`{"principals": {"x": {"token_sha256": [%q], "read": ["a", "b"], "write": ["a"], "deny": ["b"]}}}`, hashOf("t")))
	x, _ := p.Authenticate("t")
	if x.Level("a/f") != Write || x.Level("b/f") != None {
		t.Fatalf("a/f = %s, b/f = %s", x.Level("a/f"), x.Level("b/f"))
	}
}

func TestAuthenticate(t *testing.T) {
	closed := policy(t, fmt.Sprintf(`{"principals": {"ci": {"token_sha256": [%q], "read": [""]}}}`, hashOf("secret")))
	if _, err := closed.Authenticate(""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := closed.Authenticate("guess"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong token: %v", err)
	}
	open := policy(t, fmt.Sprintf(`{"principals": {"ci": {"token_sha256": [%q]}}, "anonymous": {"read": ["docs"]}}`, hashOf("secret")))
	anon, err := open.Authenticate("")
	if err != nil || anon.Name != "anonymous" || !anon.CanRead("docs/a.md") || anon.CanRead("src/a.go") || anon.WritesAnywhere() {
		t.Fatalf("anonymous = %+v, %v", anon, err)
	}
	var nobody *Principal // auth disabled
	if !nobody.CanWrite("x") || !nobody.ReadsAll("") || !nobody.CanLand("main") || nobody.CheckSee("x") != nil {
		t.Fatal("a nil principal may do anything")
	}
	token, hash, err := NewToken()
	if err != nil || !strings.HasPrefix(token, "kzt_") || hash != hashOf(token) {
		t.Fatalf("NewToken = %q %q %v", token, hash, err)
	}
}

func TestParseErrors(t *testing.T) {
	h := hashOf("t")
	for name, body := range map[string]string{
		"unknown field":   `{"principals": {}, "admins": []}`,
		"no token":        `{"principals": {"x": {"read": [""]}}}`,
		"bad hash":        `{"principals": {"x": {"token_sha256": ["abc"]}}}`,
		"shared token":    fmt.Sprintf(`{"principals": {"x": {"token_sha256": [%q]}, "y": {"token_sha256": [%q]}}}`, h, h),
		"bad path":        fmt.Sprintf(`{"principals": {"x": {"token_sha256": [%q], "read": ["a/../b"]}}}`, h),
		"anonymous token": fmt.Sprintf(`{"anonymous": {"token_sha256": [%q]}}`, h),
		"reserved name":   fmt.Sprintf(`{"principals": {"anonymous": {"token_sha256": [%q]}}}`, h),
		"empty land":      fmt.Sprintf(`{"principals": {"x": {"token_sha256": [%q], "land": [""]}}}`, h),
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
