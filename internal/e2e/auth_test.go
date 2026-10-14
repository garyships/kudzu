package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/auth"
	"github.com/garyships/kudzu/internal/client"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
)

type tokens map[string]string // principal -> token

func testPolicy(t *testing.T) (*auth.Policy, tokens) {
	t.Helper()
	tok := tokens{}
	hashes := map[string]string{}
	for _, name := range []string{"alice", "bob", "cache"} {
		token, hash, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		tok[name], hashes[name] = token, hash
	}
	policy, err := auth.Parse([]byte(fmt.Sprintf(`{
		"principals": {
			"alice": {"token_sha256": [%q], "read": [""], "deny": ["secrets"], "write": ["services/api"], "land": ["main"]},
			"bob":   {"token_sha256": [%q], "read": ["docs"]},
			"cache": {"token_sha256": [%q], "read": [""]}
		}
	}`, hashes["alice"], hashes["bob"], hashes["cache"])))
	if err != nil {
		t.Fatal(err)
	}
	return policy, tok
}

func as(t *testing.T, addr, token string) *proto.Remote {
	rem, err := proto.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	rem.Token = token
	return rem
}

func denied(t *testing.T, err error, path string) {
	t.Helper()
	var he *proto.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || he.Path != path || !strings.Contains(he.Message, path) {
		t.Fatalf("expected 403 naming %q, got %v", path, err)
	}
}

// TestAccessControl runs the whole stack with an access policy: alice may
// read everything except secrets/ and write only services/api; bob may only
// read docs/. Requests go through the daemon and the cache, which forward
// each client's token for API calls and use their own for object fetches.
func TestAccessControl(t *testing.T) {
	policy, tok := testPolicy(t)
	s := newStackAuth(t, policy)
	s.cache.Up.Token = tok["cache"]
	ctx := context.Background()
	s.seed(t)
	secret, _ := s.repo.WriteBlob(ctx, []byte("hunter2\n"))
	main, _ := s.repo.Meta.GetRef(ctx, "main")
	c, err := s.repo.Create(ctx, repo.CreateRequest{Parents: []object.ID{main}, Author: "ops", Message: "add the key",
		Changes: []object.Change{{Path: "secrets/key.txt", Mode: object.ModeFile, Blob: secret}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.Meta.CASRef(ctx, "main", main, c.ID); err != nil {
		t.Fatal(err)
	}

	// No token, or a wrong one: 401 everywhere except /healthz.
	anon := as(t, s.remote(), "")
	var refs map[string]object.ID
	if err := anon.GetJSON(ctx, "/api/refs", nil, &refs); !proto.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("anonymous request: %v", err)
	}
	if err := as(t, s.remote(), "kzt_wrong").GetJSON(ctx, "/api/refs", nil, &refs); !proto.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := as(t, s.server.URL, "").GetRaw(ctx, "/healthz", nil); err != nil {
		t.Fatalf("healthz should stay open: %v", err)
	}

	// Reads: a denied path is a 403 naming it, and listings leave it out.
	alice, bob := as(t, s.remote(), tok["alice"]), as(t, s.remote(), tok["bob"])
	_, err = alice.GetRaw(ctx, "/api/cat", url.Values{"path": {"secrets/key.txt"}})
	denied(t, err, "secrets/key.txt")
	if b, err := alice.GetRaw(ctx, "/api/cat", url.Values{"path": {"README.md"}}); err != nil || string(b) != "# monorepo\n" {
		t.Fatalf("alice cat README.md: %q %v", b, err)
	}
	var ls []object.Entry
	if err := alice.GetJSON(ctx, "/api/ls", nil, &ls); err != nil {
		t.Fatal(err)
	}
	for _, e := range ls {
		if e.Name == "secrets" {
			t.Fatal("alice's listing shows secrets/")
		}
	}
	err = alice.GetJSON(ctx, "/api/ls", url.Values{"path": {"secrets"}}, &ls)
	denied(t, err, "secrets")
	if err := bob.GetJSON(ctx, "/api/ls", nil, &ls); err != nil || len(ls) != 1 || ls[0].Name != "docs" {
		t.Fatalf("bob's root listing = %+v, %v", ls, err)
	}
	_, err = bob.GetRaw(ctx, "/api/blame", url.Values{"path": {"services/api/main.go"}})
	denied(t, err, "services/api/main.go")

	// History: a changeset that only touched unreadable paths is hidden.
	var log []struct{ Message string }
	if err := alice.GetJSON(ctx, "/api/log", nil, &log); err != nil || len(log) != 1 || log[0].Message != "initial" {
		t.Fatalf("alice's log = %+v, %v", log, err)
	}
	var show struct{}
	err = alice.GetJSON(ctx, "/api/changesets/main", nil, &show)
	denied(t, err, "changeset "+c.ID.Short())

	// Checkouts get filtered trees: alice's clone, through the daemon and the
	// cache, has everything but secrets/, and verifies every object.
	t.Setenv("KUDZU_TOKEN", tok["alice"])
	wc, err := client.Clone(ctx, s.remote(), filepath.Join(t.TempDir(), "alice"), "main", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(listFiles(t, wc.Root), " "); strings.Contains(got, "secrets") || !strings.Contains(got, "services/api/main.go") {
		t.Fatalf("alice's checkout = %s", got)
	}
	if wc.State.Tree == s.realTree(t, "main") {
		t.Fatal("alice was given the unfiltered root tree")
	}

	// Writes: alice may change services/api and land on main, nothing else.
	write(t, wc.Root, "services/api/handler.go", "package main\n\nfunc serve() { /* alice */ }\n")
	res, err := wc.Commit(ctx, "api: alice", "alice", false, "main")
	if err != nil || res.Land == nil {
		t.Fatalf("alice's commit: %+v %v", res, err)
	}
	if _, data, err := s.repo.ReadFile(ctx, res.Land.Landed, "secrets/key.txt"); err != nil || string(data) != "hunter2\n" {
		t.Fatalf("landing through a filtered checkout lost secrets/key.txt: %q %v", data, err)
	}
	write(t, wc.Root, "docs/guide.md", "rewritten by alice\n")
	_, err = wc.Commit(ctx, "docs: alice", "alice", false, "")
	denied(t, err, "docs/guide.md")
	os.WriteFile(filepath.Join(wc.Root, "docs/guide.md"), []byte("guide\n"), 0o644)

	// A rebase that would write outside alice's paths is refused too.
	head, _ := s.repo.Meta.GetRef(ctx, "main")
	docBlob, _ := s.repo.WriteBlob(ctx, []byte("by someone else\n"))
	other, err := s.repo.Create(ctx, repo.CreateRequest{Parents: []object.ID{c.ID}, Author: "carol", Message: "docs",
		Changes: []object.Change{{Path: "docs/guide.md", Mode: object.ModeFile, Blob: docBlob}}})
	if err != nil {
		t.Fatal(err)
	}
	err = alice.PostJSON(ctx, "/api/land", map[string]string{"head": other.ID.String(), "ref": "main"}, nil)
	denied(t, err, "docs/guide.md")
	if now, _ := s.repo.Meta.GetRef(ctx, "main"); now != head {
		t.Fatal("a refused land moved main")
	}

	// bob may not land, move refs or upload.
	err = bob.PostJSON(ctx, "/api/land", map[string]string{"head": "main", "ref": "main"}, nil)
	denied(t, err, "main")
	err = bob.PostJSON(ctx, "/api/refs/main", map[string]any{"old": head, "new": ""}, nil)
	denied(t, err, "main")
	_, err = bob.Missing(ctx, []object.ID{secret})
	denied(t, err, "")
}

// realTree is the unfiltered root tree of rev.
func (s *stack) realTree(t *testing.T, rev string) object.ID {
	ctx := context.Background()
	id, err := s.repo.Resolve(ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := s.repo.TreeOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}
