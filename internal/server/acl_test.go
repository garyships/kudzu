package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/auth"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

// rig is a server enforcing a policy with two principals: alice, whose rules
// each test chooses, and admin, who may do anything. main holds one
// changeset with files in services/api, secrets and docs.
type rig struct {
	r     *repo.Repo
	alice *proto.Remote
	admin *proto.Remote
	main  object.ID
}

func newRig(t *testing.T, aliceRules string) *rig {
	t.Helper()
	ctx := context.Background()
	ta, ha, _ := auth.NewToken()
	tb, hb, _ := auth.NewToken()
	policy, err := auth.Parse([]byte(fmt.Sprintf(`{"principals": {
		"alice": {"token_sha256": [%q], %s},
		"admin": {"token_sha256": [%q], "read": [""], "write": [""], "land": ["*"]}
	}}`, ha, aliceRules, hb)))
	if err != nil {
		t.Fatal(err)
	}
	r := repo.New(storetest.Meta(t), &store.Cached{Mem: store.NewMem(64 << 20), Backing: storetest.Objects(t)})
	srv := New(r, log.New(io.Discard, "", 0))
	srv.Auth = policy
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	g := &rig{r: r}
	g.main = g.commit(t, object.Zero, "initial", map[string]string{
		"README.md":            "# monorepo\n",
		"services/api/main.go": "package main\n",
		"secrets/key.txt":      "hunter2\n",
		"docs/guide.md":        "guide\n",
	})
	if err := r.Meta.CASRef(ctx, "main", object.Zero, g.main); err != nil {
		t.Fatal(err)
	}
	dial := func(token string) *proto.Remote {
		rem, _ := proto.Dial(hs.URL)
		rem.Token = token
		return rem
	}
	g.alice, g.admin = dial(ta), dial(tb)
	return g
}

// commit creates a changeset directly in the repository (as an admin would).
func (g *rig) commit(t *testing.T, parent object.ID, msg string, set map[string]string) object.ID {
	t.Helper()
	ctx := context.Background()
	var changes []object.Change
	for p, content := range set {
		id, _ := g.r.WriteBlob(ctx, []byte(content))
		changes = append(changes, object.Change{Path: p, Mode: object.ModeFile, Blob: id})
	}
	var parents []object.ID
	if !parent.IsZero() {
		parents = []object.ID{parent}
	}
	c, err := g.r.Create(ctx, repo.CreateRequest{Parents: parents, Author: "admin", Message: msg, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (g *rig) tip(t *testing.T) object.ID {
	tip, err := g.r.Meta.GetRef(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	return tip
}

func status(err error) int {
	var he *proto.HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

const aliceAPI = `"read": [""], "deny": ["secrets"], "write": ["services/api"], "land": ["main"]`

// TestRefMovesCheckContent: moving a ref changes its content as much as a new
// changeset does, so a land that fast-forwards or a compare-and-swap needs
// write access to every path whose content changes.
func TestRefMovesCheckContent(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, aliceAPI)
	start := g.main

	// A "merge" whose first parent is a root alice wrote and whose second is
	// main changes nothing relative to its first parent, and landing it is a
	// fast-forward, yet it would replace main's whole tree.
	blob := object.Hash(object.KindBlob, []byte("x\n"))
	if err := g.alice.Upload(ctx, map[object.ID][]byte{blob: object.Encode(object.KindBlob, []byte("x\n"))}); err != nil {
		t.Fatal(err)
	}
	var root struct{ ID object.ID }
	if err := g.alice.PostJSON(ctx, "/api/changesets", map[string]any{
		"parents": []object.ID{}, "author": "alice", "message": "root",
		"changes": []object.Change{{Path: "services/api/x", Mode: object.ModeFile, Blob: blob}},
	}, &root); err != nil {
		t.Fatal(err)
	}
	err := g.alice.PostJSON(ctx, "/api/changesets", map[string]any{
		"parents": []object.ID{root.ID, g.main}, "author": "alice", "message": "merge", "land": "main",
	}, nil)
	if status(err) != http.StatusForbidden || strings.Contains(err.Error(), "secrets") {
		t.Fatalf("second-parent land: %v", err)
	}

	// Fast-forwarding main to someone else's change outside alice's paths.
	theirs := g.commit(t, start, "docs", map[string]string{"docs/guide.md": "new guide\n"})
	err = g.alice.PostJSON(ctx, "/api/land", map[string]string{"head": theirs.String(), "ref": "main"}, nil)
	if status(err) != http.StatusForbidden || !strings.Contains(err.Error(), "docs/guide.md") {
		t.Fatalf("fast-forward land of docs/: %v", err)
	}

	// Rolling main back over a change to secrets/ with a compare-and-swap.
	secret := g.commit(t, start, "rotate", map[string]string{"secrets/key.txt": "rotated\n"})
	if err := g.r.Meta.CASRef(ctx, "main", start, secret); err != nil {
		t.Fatal(err)
	}
	err = g.alice.PostJSON(ctx, "/api/refs/main", map[string]any{"old": secret, "new": start.String()}, nil)
	if status(err) != http.StatusForbidden || strings.Contains(err.Error(), "secrets") {
		t.Fatalf("rollback by CAS: %v", err)
	}
	if g.tip(t) != secret {
		t.Fatal("a refused ref move moved main")
	}

	// What alice may write still moves: a fast-forward and a CAS within
	// services/api.
	mine := g.commit(t, secret, "api", map[string]string{"services/api/main.go": "package main // a\n"})
	var res repo.LandResult
	if err := g.alice.PostJSON(ctx, "/api/land", map[string]string{"head": mine.String(), "ref": "main"}, &res); err != nil || res.Landed != mine {
		t.Fatalf("alice's own land: %+v %v", res, err)
	}
	if err := g.alice.PostJSON(ctx, "/api/refs/main", map[string]any{"old": mine, "new": secret.String()}, nil); err != nil {
		t.Fatalf("alice's own CAS: %v", err)
	}
	// admin is not restricted at all.
	if err := g.admin.PostJSON(ctx, "/api/refs/main", map[string]any{"old": secret, "new": start.String()}, nil); err != nil {
		t.Fatalf("admin CAS: %v", err)
	}
}

// TestNoGuessingHiddenContent: whether a write to an unreadable path is
// refused must not depend on what the path holds.
func TestNoGuessingHiddenContent(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, aliceAPI)
	try := func(guess string) error {
		b := []byte(guess)
		id := object.Hash(object.KindBlob, b)
		if err := g.alice.Upload(ctx, map[object.ID][]byte{id: object.Encode(object.KindBlob, b)}); err != nil {
			t.Fatal(err)
		}
		return g.alice.PostJSON(ctx, "/api/changesets", map[string]any{
			"parents": []object.ID{g.main}, "author": "alice", "message": "probe",
			"changes": []object.Change{{Path: "secrets/key.txt", Mode: object.ModeFile, Blob: id}},
		}, nil)
	}
	wrong, right := try("password\n"), try("hunter2\n")
	if status(wrong) != http.StatusForbidden || status(right) != http.StatusForbidden || wrong.Error() != right.Error() {
		t.Fatalf("wrong guess: %v; right guess: %v", wrong, right)
	}
}

// TestErrorsHideUnreadablePaths: merges and rebases against someone else's
// changes must not name paths the caller may not read, in permission errors
// or in conflicts.
func TestErrorsHideUnreadablePaths(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, aliceAPI)
	ops := g.commit(t, g.main, "ops", map[string]string{"secrets/new-hidden-name.pem": "pem\n", "secrets/key.txt": "ops\n"})
	mine := g.commit(t, g.main, "mine", map[string]string{"services/api/main.go": "package main // mine\n"})
	err := g.alice.PostJSON(ctx, "/api/merge", map[string]any{"ours": mine.String(), "theirs": ops.String()}, nil)
	if status(err) != http.StatusForbidden || strings.Contains(err.Error(), "secrets") {
		t.Fatalf("merge: %v", err)
	}

	// Rebasing ops onto a main that changed the same secret conflicts; the
	// conflict is reported without the path.
	other := g.commit(t, g.main, "other", map[string]string{"secrets/key.txt": "other\n", "services/api/main.go": "package main // other\n"})
	err = g.alice.PostJSON(ctx, "/api/rebase", map[string]any{"head": ops.String(), "onto": other.String()}, nil)
	var he *proto.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || len(he.Conflicts) != 0 || strings.Contains(he.Message, "secrets") {
		t.Fatalf("rebase conflict: %+v", err)
	}
	if !strings.Contains(he.Message, "1 paths you may not read") {
		t.Fatalf("conflict message: %q", he.Message)
	}
	// Conflicts on readable paths are still named.
	mine2 := g.commit(t, g.main, "mine2", map[string]string{"services/api/main.go": "package main // mine2\n"})
	err = g.alice.PostJSON(ctx, "/api/rebase", map[string]any{"head": mine2.String(), "onto": other.String()}, nil)
	if !errors.As(err, &he) || he.Status != http.StatusConflict || len(he.Conflicts) != 1 || he.Conflicts[0] != "services/api/main.go" {
		t.Fatalf("readable conflict: %+v", err)
	}
}

// TestWALSkipsHiddenEntries: a reader that may not see some changesets still
// gets a position for each, so its cursor moves past them.
func TestWALSkipsHiddenEntries(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, `"read": ["docs"]`)
	prev := g.main
	for i := 0; i < 4; i++ {
		prev = g.commit(t, prev, "secret", map[string]string{"secrets/key.txt": fmt.Sprintf("s%d\n", i)})
	}
	docs := g.commit(t, prev, "docs", map[string]string{"docs/guide.md": "new docs\n"})
	type entry struct {
		Seq     int64     `json:"seq"`
		ID      object.ID `json:"id"`
		Hidden  bool      `json:"hidden"`
		Message string    `json:"message"`
		Changes []object.Change
	}
	var found *entry
	since := int64(0)
	for polls := 0; polls < 10 && found == nil; polls++ {
		var page []entry
		if err := g.alice.GetJSON(ctx, "/api/wal", url.Values{"since": {fmt.Sprint(since)}, "limit": {"2"}}, &page); err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for i, e := range page {
			if e.Hidden && (e.Message != "" || len(e.Changes) > 0) {
				t.Fatalf("hidden entry has content: %+v", e)
			}
			if e.ID == docs {
				found = &page[i]
			}
		}
		since = page[len(page)-1].Seq
	}
	if found == nil || found.Hidden || len(found.Changes) != 1 {
		t.Fatalf("never reached the docs changeset: %+v", found)
	}
}

func TestRefNamesAreValidated(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, aliceAPI)
	for _, name := range []string{"rel^x", "a..b", "x~1", "a/.hidden", "b.lock", "with space"} {
		err := g.admin.PostJSON(ctx, "/api/refs/"+url.PathEscape(name), map[string]any{"new": "main"}, nil)
		if status(err) != http.StatusBadRequest {
			t.Errorf("creating ref %q: %v", name, err)
		}
	}
	if err := g.admin.PostJSON(ctx, "/api/land", map[string]string{"head": "main", "ref": "x:y"}, nil); status(err) != http.StatusBadRequest {
		t.Errorf("landing on a new ref named x:y: %v", err)
	}
	for _, name := range []string{"users/alice/feature-1", "release-2.0"} {
		if err := g.admin.PostJSON(ctx, "/api/refs/"+name, map[string]any{"new": "main"}, nil); err != nil {
			t.Errorf("creating ref %q: %v", name, err)
		}
	}
}
