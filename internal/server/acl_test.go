package server

import (
	"context"
	"encoding/json"
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
	srv   *Server
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
	g := &rig{r: r, srv: srv}
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
	// fast-forward, yet it would replace main's whole tree. alice may not
	// create it herself (TestNoForgedMergeParents), but may try to land one
	// that someone who writes everything made.
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
	merge, err := g.r.Create(ctx, repo.CreateRequest{Parents: []object.ID{root.ID, g.main}, Author: "alice", Message: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	err = g.alice.PostJSON(ctx, "/api/land", map[string]string{"head": merge.ID.String(), "ref": "main"}, nil)
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

// TestNoForgedMergeParents: a principal whose writes are checked may not name
// a second parent. A "merge" of someone's pending changeset that changes
// nothing passes every path check, yet from then on that changeset counts as
// landed: its owner's land would succeed without landing anything, and the
// land of a stack on top of it would leave it out.
func TestNoForgedMergeParents(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, aliceAPI)
	start := g.main
	x1 := g.commit(t, start, "rotate key", map[string]string{"secrets/key.txt": "rotated\n"})
	x2 := g.commit(t, x1, "docs", map[string]string{"docs/guide.md": "new guide\n"})
	for _, pending := range []object.ID{x1, x2} {
		err := g.alice.PostJSON(ctx, "/api/changesets", map[string]any{
			"parents": []object.ID{start, pending}, "author": "alice", "message": "merge", "land": "main",
		}, nil)
		if status(err) != http.StatusForbidden {
			t.Fatalf("merge naming %s: %v", pending.Short(), err)
		}
	}
	var res repo.LandResult
	if err := g.admin.PostJSON(ctx, "/api/land", map[string]string{"head": x2.String(), "ref": "main"}, &res); err != nil || res.Landed != x2 {
		t.Fatalf("landing the stack: %+v %v", res, err)
	}

	// alice merges through /api/merge, which checks what the merge changes.
	api := g.commit(t, start, "api", map[string]string{"services/api/main.go": "package main // api\n"})
	var merged repo.Created
	if err := g.alice.PostJSON(ctx, "/api/merge", map[string]any{"ours": "main", "theirs": api.String()}, &merged); err != nil {
		t.Fatalf("alice's merge: %v", err)
	}
	// Whoever may write everything may still name parents, but not one twice.
	if err := g.admin.PostJSON(ctx, "/api/changesets", map[string]any{
		"parents": []object.ID{x2, api}, "author": "admin", "message": "merge",
	}, nil); err != nil {
		t.Fatalf("admin's merge: %v", err)
	}
	err := g.admin.PostJSON(ctx, "/api/changesets", map[string]any{
		"parents": []object.ID{x2, x2}, "author": "admin", "message": "merge",
	}, nil)
	if status(err) != http.StatusBadRequest {
		t.Fatalf("parent named twice: %v", err)
	}
}

// TestOnlyWritersCreate: a principal that may not write anywhere cannot
// create, rebase or merge, although a changeset with no changes passes every
// path check.
func TestOnlyWritersCreate(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, `"read": ["docs"], "land": ["main"]`)
	empty := g.commit(t, g.main, "empty", nil)
	docs := g.commit(t, g.main, "docs", map[string]string{"docs/guide.md": "new guide\n"})
	if err := g.r.Meta.CASRef(ctx, "main", g.main, docs); err != nil {
		t.Fatal(err)
	}
	before, err := g.r.Meta.WAL(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]any{
		"/api/changesets": map[string]any{"parents": []object.ID{docs}, "author": "Release Manager", "message": "release"},
		"/api/rebase":     map[string]string{"head": empty.String(), "onto": "main"},
		"/api/merge":      map[string]string{"ours": "main", "theirs": empty.String()},
	} {
		if err := g.alice.PostJSON(ctx, path, body, nil); status(err) != http.StatusForbidden {
			t.Errorf("%s: %v", path, err)
		}
	}
	if after, err := g.r.Meta.WAL(ctx, 0, 100); err != nil || len(after) != len(before) {
		t.Fatalf("the log grew from %d to %d entries (%v)", len(before), len(after), err)
	}
}

// TestCreateLimits: what a changeset from the API may hold is bounded.
func TestCreateLimits(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, aliceAPI)
	many := make([]object.ID, maxParents+1)
	for i := range many {
		many[i] = g.main
	}
	one := []object.ID{g.main}
	for what, req := range map[string]map[string]any{
		"parents":          {"parents": many, "author": "a", "message": "m"},
		"bytes of message": {"parents": one, "author": "a", "message": strings.Repeat("m", maxMessage+1)},
		"bytes of author":  {"parents": one, "author": strings.Repeat("a", maxAuthor+1), "message": "m"},
		"bytes of extra":   {"parents": one, "author": "a", "message": "m", "extra": map[string]string{"k": strings.Repeat("v", maxExtra)}},
	} {
		err := g.admin.PostJSON(ctx, "/api/changesets", req, nil)
		var he *proto.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusBadRequest || !strings.Contains(he.Message, what+" (at most") {
			t.Errorf("too many %s: %v", what, err)
		}
	}
	if err := g.admin.PostJSON(ctx, "/api/changesets", map[string]any{"parents": one, "author": "a", "message": strings.Repeat("m", maxMessage)}, nil); err != nil {
		t.Errorf("a message at the limit: %v", err)
	}
	other := g.commit(t, g.main, "docs", map[string]string{"docs/guide.md": "new guide\n"})
	err := g.admin.PostJSON(ctx, "/api/merge", map[string]any{"ours": "main", "theirs": other.String(), "message": strings.Repeat("m", maxMessage+1)}, nil)
	if status(err) != http.StatusBadRequest {
		t.Errorf("a merge with too long a message: %v", err)
	}
}

// watchedBody is a request body that records whether anything read it.
type watchedBody struct{ read bool }

func (b *watchedBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

// post sends a request straight to the server's handler as alice.
func (g *rig) post(path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, body)
	req.Header.Set("Authorization", "Bearer "+g.alice.Token)
	rec := httptest.NewRecorder()
	g.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestRequestBodies: a caller that may not make a request is refused before
// its body is read, and no endpoint reads more of a body than its requests
// need, so large bodies cannot tie up the server's memory.
func TestRequestBodies(t *testing.T) {
	g := newRig(t, `"read": ["docs"], "land": ["users/alice/*"]`)
	for _, path := range []string{"/api/changesets", "/api/rebase", "/api/merge", "/api/refs/main"} {
		body := &watchedBody{}
		if rec := g.post(path, body); rec.Code != http.StatusForbidden || body.read {
			t.Errorf("%s: status %d, body read: %v", path, rec.Code, body.read)
		}
	}

	g = newRig(t, `"read": [""], "write": ["services/api"], "land": ["main", "users/alice/*"]`)
	rev := strings.Repeat("a", maxSmallBody)
	for path, body := range map[string]string{
		"/api/land":                 `{"head": "` + rev + `", "ref": "main"}`,
		"/api/rebase":               `{"head": "` + rev + `", "onto": "main"}`,
		"/api/refs/users/alice/new": `{"new": "` + rev + `"}`,
		"/api/merge":                `{"ours": "main", "theirs": "main", "message": "` + strings.Repeat("m", maxMergeBody) + `"}`,
	} {
		if rec := g.post(path, strings.NewReader(body)); rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s with a %d KiB body: status %d: %s", path, len(body)>>10, rec.Code, rec.Body)
		}
	}
}

// TestInternalErrorsStayInTheLog: a backend error can name an object's key,
// which for a caller with restricted read access may be the real ID of a tree
// it only gets filtered, and so a way to fetch what it may not read. Such
// errors go to the log; the caller gets an ID that finds them there.
func TestInternalErrorsStayInTheLog(t *testing.T) {
	ctx := context.Background()
	objs := &brokenObjects{Objects: store.NewMem(4 << 20)}
	meta := storetest.Meta(t)
	r := repo.New(meta, objs)
	var changes []object.Change
	for p, content := range map[string]string{"README.md": "hi\n", "secrets/key.txt": "hunter2\n"} {
		id, _ := r.WriteBlob(ctx, []byte(content))
		changes = append(changes, object.Change{Path: p, Mode: object.ModeFile, Blob: id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Author: "admin", Message: "init", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	token, hash, _ := auth.NewToken()
	policy, err := auth.Parse([]byte(fmt.Sprintf(`{"principals": {"alice": {"token_sha256": [%q], %s}}}`, hash, aliceAPI)))
	if err != nil {
		t.Fatal(err)
	}
	// A server with cold caches, as after a restart, while the disk fails
	// to read the root tree.
	var logged strings.Builder
	srv := New(repo.New(meta, objs), log.New(&logged, "", 0))
	srv.Auth = policy
	objs.broken = c.Tree

	req := httptest.NewRequest("GET", "/api/resolve?rev=main", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var resp struct{ Error string }
	json.Unmarshal(rec.Body.Bytes(), &resp)
	key := c.Tree.String()[2:]
	id, ok := strings.CutPrefix(resp.Error, "internal error (request ")
	if rec.Code != http.StatusInternalServerError || !ok || strings.Contains(rec.Body.String(), key) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	id = strings.TrimSuffix(id, ")")
	if !strings.Contains(logged.String(), "request "+id+"): read objects/") || !strings.Contains(logged.String(), key) {
		t.Fatalf("log for request %s: %q", id, logged.String())
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

// TestPathErrorsHideUnreadablePaths: a change that does not fit the tree is
// refused naming the path in the way, unless the caller may not read it.
func TestPathErrorsHideUnreadablePaths(t *testing.T) {
	ctx := context.Background()
	g := newRig(t, `"read": [""], "deny": ["secrets"], "write": ["services/api", "secrets/key.txt/sub"]`)
	blob := object.Hash(object.KindBlob, []byte("x\n"))
	if err := g.alice.Upload(ctx, map[object.ID][]byte{blob: object.Encode(object.KindBlob, []byte("x\n"))}); err != nil {
		t.Fatal(err)
	}
	create := func(path string) error {
		return g.alice.PostJSON(ctx, "/api/changesets", map[string]any{
			"parents": []object.ID{g.main}, "author": "alice", "message": "m",
			"changes": []object.Change{{Path: path, Mode: object.ModeFile, Blob: blob}},
		}, nil)
	}
	if err := create("secrets/key.txt/sub/x"); status(err) != http.StatusBadRequest || strings.Contains(err.Error(), "secrets") {
		t.Fatalf("below a hidden file: %v", err)
	}
	if err := create("services/api/main.go/x"); status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "services/api/main.go: is a file") {
		t.Fatalf("below a readable file: %v", err)
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
	for _, name := range []string{"rel^x", "a..b", "x~1", "a/.hidden", "b.lock", "with space", strings.Repeat("r", 256)} {
		err := g.admin.PostJSON(ctx, "/api/refs/"+url.PathEscape(name), map[string]any{"new": "main"}, nil)
		if status(err) != http.StatusBadRequest {
			t.Errorf("creating ref %q: %v", name, err)
		}
	}
	if err := g.admin.PostJSON(ctx, "/api/land", map[string]string{"head": "main", "ref": "x:y"}, nil); status(err) != http.StatusBadRequest {
		t.Errorf("landing on a new ref named x:y: %v", err)
	}
	for _, name := range []string{"users/alice/feature-1", "release-2.0", strings.Repeat("r", 255)} {
		if err := g.admin.PostJSON(ctx, "/api/refs/"+name, map[string]any{"new": "main"}, nil); err != nil {
			t.Errorf("creating ref %q: %v", name, err)
		}
	}
}
