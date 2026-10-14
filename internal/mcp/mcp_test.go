package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/garyships/kudzu/internal/client"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
)

type env struct {
	repo   *repo.Repo
	remote *proto.Remote
	url    string
	first  object.ID // the initial changeset
}

// newEnv starts an authoritative server with a small monorepo on main:
// "initial" by seed, then "api: v2" by alice.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	meta, err := store.OpenSQLite(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })
	objs, err := store.OpenFS(filepath.Join(dir, "objects"), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := repo.New(meta, &store.Cached{Mem: store.NewMem(64 << 20), Backing: objs})
	srv := httptest.NewServer(server.New(r, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	rem, err := proto.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{repo: r, remote: rem, url: srv.URL}

	var big strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&big, "line %d\n", i)
	}
	e.first = e.commit(t, object.Zero, "seed", "initial", map[string]string{
		"README.md":               "# monorepo\n",
		"services/api/main.go":    "package main\n\nfunc main() {\n\tserve()\n}\n",
		"services/api/handler.go": "package main\n\nfunc serve() {}\n",
		"tools/run.sh":            "#!/bin/sh\necho run\n",
		"assets/logo.bin":         "\x89PNG\x00\x01\x02",
		"big.txt":                 big.String(),
	})
	e.commit(t, e.first, "alice", "api: v2", map[string]string{
		"services/api/handler.go": "package main\n\nfunc serve() { println(\"v2\") }\n",
	})
	return e
}

// commit creates a changeset on parent through the repo and moves main to it.
func (e *env) commit(t *testing.T, parent object.ID, author, msg string, files map[string]string) object.ID {
	t.Helper()
	ctx := context.Background()
	var changes []object.Change
	for p, c := range files {
		id, err := e.repo.WriteBlob(ctx, []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		mode := object.ModeFile
		if strings.HasSuffix(p, ".sh") {
			mode = object.ModeExec
		}
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	var parents []object.ID
	if !parent.IsZero() {
		parents = []object.ID{parent}
	}
	c, err := e.repo.Create(ctx, repo.CreateRequest{Parents: parents, Author: author, Message: msg, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.repo.Meta.CASRef(ctx, "main", parent, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func (e *env) connect(t *testing.T, opts Options) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := NewServer(e.remote, opts).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

// ok calls a tool that must succeed and decodes its JSON summary into out.
// It returns the text blocks after the summary.
func ok(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out any) []string {
	t.Helper()
	res := call(t, cs, name, args)
	texts := texts(res)
	if res.IsError {
		t.Fatalf("%s(%v) failed: %s", name, args, strings.Join(texts, "\n"))
	}
	if out != nil {
		if err := json.Unmarshal([]byte(texts[0]), out); err != nil {
			t.Fatalf("%s: bad JSON %q: %v", name, texts[0], err)
		}
	}
	return texts[1:]
}

// fails calls a tool that must return a tool error and returns its text.
func fails(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res := call(t, cs, name, args)
	if !res.IsError {
		t.Fatalf("%s(%v) should fail, got %s", name, args, strings.Join(texts(res), "\n"))
	}
	return strings.Join(texts(res), "\n")
}

func texts(res *sdk.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

var writeTools = []string{"kudzu_land", "kudzu_rebase", "kudzu_wc_commit", "kudzu_wc_update", "kudzu_write"}

func toolNames(t *testing.T, cs *sdk.ClientSession) map[string]*sdk.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func TestListTools(t *testing.T) {
	e := newEnv(t)
	all := toolNames(t, e.connect(t, Options{}))
	want := []string{"kudzu_ancestry", "kudzu_blame", "kudzu_cat", "kudzu_diff", "kudzu_land", "kudzu_log", "kudzu_ls",
		"kudzu_rebase", "kudzu_refs", "kudzu_resolve", "kudzu_show", "kudzu_stats", "kudzu_wc_commit", "kudzu_wc_status",
		"kudzu_wc_update", "kudzu_write"}
	var got []string
	for name, tool := range all {
		got = append(got, name)
		if tool.Description == "" || tool.Annotations == nil {
			t.Errorf("%s: missing description or annotations", name)
			continue
		}
		write := slices.Contains(writeTools, name)
		if tool.Annotations.ReadOnlyHint == write {
			t.Errorf("%s: readOnlyHint = %v", name, tool.Annotations.ReadOnlyHint)
		}
		if write && tool.Annotations.DestructiveHint == nil {
			t.Errorf("%s: write tool without destructiveHint", name)
		}
		schema, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(schema), `"null"`) {
			t.Errorf("%s: schema allows null: %s", name, schema)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v\nwant    %v", got, want)
	}

	ro := toolNames(t, e.connect(t, Options{ReadOnly: true}))
	for _, name := range writeTools {
		if ro[name] != nil {
			t.Errorf("read-only server exposes %s", name)
		}
	}
	if len(ro) != len(want)-len(writeTools) {
		t.Errorf("read-only server has %d tools", len(ro))
	}
	_, err := e.connect(t, Options{ReadOnly: true}).CallTool(context.Background(), &sdk.CallToolParams{
		Name: "kudzu_write", Arguments: map[string]any{"parent": "main", "message": "x", "delete": []string{"README.md"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("read-only server ran kudzu_write: %v", err)
	}
}

func TestReadTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, Options{ReadOnly: true})
	main, _ := e.repo.Resolve(context.Background(), "main")

	var resolved struct{ Changeset, Tree string }
	ok(t, cs, "kudzu_resolve", map[string]any{"rev": "main~1"}, &resolved)
	if resolved.Changeset != e.first.String() || resolved.Tree == "" {
		t.Fatalf("resolve main~1 = %+v", resolved)
	}

	var logOut struct {
		Head       string `json:"head"`
		More       bool   `json:"more"`
		Changesets []struct {
			ID, Author, Message, Date string
			Parents                   []string
		} `json:"changesets"`
	}
	ok(t, cs, "kudzu_log", nil, &logOut)
	if logOut.Head != main.String() || len(logOut.Changesets) != 2 || logOut.Changesets[0].Message != "api: v2" || logOut.More {
		t.Fatalf("log = %+v", logOut)
	}
	ok(t, cs, "kudzu_log", map[string]any{"limit": 1}, &logOut)
	if len(logOut.Changesets) != 1 || !logOut.More {
		t.Fatalf("log limit 1 = %+v", logOut)
	}
	ok(t, cs, "kudzu_log", map[string]any{"path": "services/api/main.go"}, &logOut)
	if len(logOut.Changesets) != 1 || logOut.Changesets[0].Message != "initial" {
		t.Fatalf("path log = %+v", logOut)
	}
	if msg := fails(t, cs, "kudzu_log", map[string]any{"path": "no/such/file"}); !strings.Contains(msg, "not found") {
		t.Fatalf("log of a missing path: %s", msg)
	}

	var blame struct {
		TotalLines int  `json:"total_lines"`
		Truncated  bool `json:"truncated"`
	}
	body := ok(t, cs, "kudzu_blame", map[string]any{"path": "services/api/handler.go"}, &blame)
	lines := strings.Split(strings.TrimSpace(body[0]), "\n")
	if blame.TotalLines != 3 || len(lines) != 3 || !strings.Contains(lines[0], "seed") || !strings.Contains(lines[2], "alice") ||
		!strings.Contains(lines[2], `println("v2")`) {
		t.Fatalf("blame = %+v\n%s", blame, body[0])
	}
	body = ok(t, cs, "kudzu_blame", map[string]any{"path": "big.txt", "start_line": 10, "end_line": 12}, &blame)
	if blame.TotalLines != 3000 || !blame.Truncated || strings.Count(body[0], "\n") != 3 || !strings.Contains(body[0], "line 10\n") {
		t.Fatalf("blame range = %+v\n%s", blame, body[0])
	}

	var diff struct {
		From, To string
		Files    []struct{ Path, Status string }
		Trunc    bool `json:"patch_truncated"`
	}
	body = ok(t, cs, "kudzu_diff", map[string]any{"to": "main"}, &diff)
	if diff.From != e.first.String() || len(diff.Files) != 1 || diff.Files[0].Status != "M" || diff.Trunc ||
		!strings.Contains(body[0], `+func serve() { println("v2") }`) {
		t.Fatalf("diff = %+v\n%v", diff, body)
	}
	body = ok(t, cs, "kudzu_diff", map[string]any{"from": "main", "to": "main~1", "patch": false}, &diff)
	if len(body) != 0 || len(diff.Files) != 1 {
		t.Fatalf("diff without patch = %+v %v", diff, body)
	}
	body = ok(t, cs, "kudzu_diff", map[string]any{"to": "main~1", "max_patch_bytes": 200}, &diff)
	if !diff.Trunc || len(body[0]) > 200 {
		t.Fatalf("patch not truncated: %d bytes", len(body[0]))
	}

	var cat struct {
		Changeset, Mode, Type string
		Size                  int
		Binary, Truncated     bool
	}
	body = ok(t, cs, "kudzu_cat", map[string]any{"path": "services/api/handler.go", "rev": "main~1"}, &cat)
	if body[0] != "package main\n\nfunc serve() {}\n" || cat.Changeset != e.first.String() || cat.Mode != "100644" || cat.Truncated {
		t.Fatalf("cat = %+v %q", cat, body)
	}
	ok(t, cs, "kudzu_cat", map[string]any{"path": "tools/run.sh"}, &cat)
	if cat.Type != "executable" {
		t.Fatalf("cat run.sh = %+v", cat)
	}
	body = ok(t, cs, "kudzu_cat", map[string]any{"path": "assets/logo.bin"}, &cat)
	if !cat.Binary || len(body) != 0 {
		t.Fatalf("binary cat = %+v %q", cat, body)
	}
	body = ok(t, cs, "kudzu_cat", map[string]any{"path": "big.txt", "start_line": 2999}, &cat)
	if body[0] != "line 2999\nline 3000\n" || !cat.Truncated {
		t.Fatalf("cat range = %+v %q", cat, body)
	}
	body = ok(t, cs, "kudzu_cat", map[string]any{"path": "big.txt", "max_bytes": 100}, &cat)
	if len(body[0]) > 100 || !strings.HasSuffix(body[0], "\n") || !cat.Truncated {
		t.Fatalf("cat max_bytes = %+v %q", cat, body)
	}
	if msg := fails(t, cs, "kudzu_cat", map[string]any{"path": "services"}); !strings.Contains(msg, "directory") {
		t.Fatalf("cat of a directory: %s", msg)
	}

	var ls struct {
		Entries []struct{ Name, Type string }
	}
	ok(t, cs, "kudzu_ls", map[string]any{}, &ls)
	var names []string
	for _, en := range ls.Entries {
		names = append(names, en.Name+":"+en.Type)
	}
	if strings.Join(names, " ") != "README.md:file assets:dir big.txt:file services:dir tools:dir" {
		t.Fatalf("ls = %v", names)
	}

	var show struct {
		Changeset struct{ ID, Author, Message string }
		Seq       int
		Changes   []struct{ Path, Mode string }
	}
	ok(t, cs, "kudzu_show", map[string]any{"rev": main.String()[:10]}, &show)
	if show.Changeset.ID != main.String() || show.Changeset.Author != "alice" || show.Seq != 2 || len(show.Changes) != 1 {
		t.Fatalf("show = %+v", show)
	}

	var anc struct {
		MergeBase   string `json:"merge_base"`
		AIsAncestor bool   `json:"a_is_ancestor_of_b"`
	}
	ok(t, cs, "kudzu_ancestry", map[string]any{"a": "main~1", "b": "main"}, &anc)
	if anc.MergeBase != e.first.String() || !anc.AIsAncestor {
		t.Fatalf("ancestry = %+v", anc)
	}

	var refs struct {
		Refs []struct{ Name, Changeset string }
	}
	ok(t, cs, "kudzu_refs", nil, &refs)
	if len(refs.Refs) != 1 || refs.Refs[0].Name != "main" || refs.Refs[0].Changeset != main.String() {
		t.Fatalf("refs = %+v", refs)
	}
	var stats struct{ Tier string }
	ok(t, cs, "kudzu_stats", nil, &stats)
	if stats.Tier != "server" {
		t.Fatalf("stats = %+v", stats)
	}

	// Bad arguments and bad revisions are tool errors, not protocol errors.
	if msg := fails(t, cs, "kudzu_cat", map[string]any{}); !strings.Contains(msg, "path") {
		t.Fatalf("missing argument: %s", msg)
	}
	if msg := fails(t, cs, "kudzu_log", map[string]any{"limit": "ten"}); !strings.Contains(msg, "invalid arguments") {
		t.Fatalf("wrong type: %s", msg)
	}
	if msg := fails(t, cs, "kudzu_resolve", map[string]any{"rev": "nope"}); !strings.Contains(msg, "unknown revision") {
		t.Fatalf("bad revision: %s", msg)
	}
}

func TestWriteAndLand(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, Options{Author: "Agent <agent@example.com>"})
	ctx := context.Background()
	base, _ := e.repo.Resolve(ctx, "main")

	var out struct {
		Changeset     string `json:"changeset"`
		Parent        string `json:"parent"`
		BlobsUploaded int    `json:"blobs_uploaded"`
		Land          *struct {
			Ref, Landed string
			Rebased     bool
		} `json:"land"`
	}
	ok(t, cs, "kudzu_write", map[string]any{
		"parent":  base.String(),
		"message": "tools: v2",
		"files": []map[string]any{
			{"path": "tools/run.sh", "content": "#!/bin/sh\necho run v2\n"},
			{"path": "new/dir/file.txt", "content": "hello\n"},
			{"path": "README.md", "content": "# monorepo\n"}, // unchanged: no upload
		},
		"delete": []string{"assets/logo.bin"},
		"land":   "main",
	}, &out)
	if out.Land == nil || out.Land.Landed != out.Changeset || out.Land.Rebased || out.Parent != base.String() || out.BlobsUploaded != 2 {
		t.Fatalf("write = %+v", out)
	}
	tip, _ := e.repo.Meta.GetRef(ctx, "main")
	if tip.String() != out.Changeset {
		t.Fatalf("main = %s, want %s", tip.Short(), out.Changeset)
	}
	c, _ := e.repo.Changeset(ctx, tip)
	if c.Author != "Agent <agent@example.com>" || c.Message != "tools: v2" || len(c.Changes) != 3 {
		t.Fatalf("changeset = %+v", c)
	}
	e2, data, err := e.repo.ReadFile(ctx, tip, "tools/run.sh")
	if err != nil || e2.Mode != object.ModeExec || string(data) != "#!/bin/sh\necho run v2\n" {
		t.Fatalf("run.sh mode %o, %q, %v (executable bit must be kept)", e2.Mode, data, err)
	}
	if _, _, err := e.repo.ReadFile(ctx, tip, "assets/logo.bin"); err == nil {
		t.Fatal("logo.bin not deleted")
	}

	// A write on the old base that touches another file lands by rebasing.
	ok(t, cs, "kudzu_write", map[string]any{
		"parent":  base.String(),
		"message": "api: main",
		"author":  "Bob <bob@example.com>",
		"files":   []map[string]any{{"path": "services/api/main.go", "content": "package main\n", "executable": true}},
		"land":    "main",
	}, &out)
	if !out.Land.Rebased {
		t.Fatalf("expected a rebased land: %+v", out.Land)
	}
	tip, _ = e.repo.Meta.GetRef(ctx, "main")
	if e3, _, _ := e.repo.ReadFile(ctx, tip, "services/api/main.go"); e3.Mode != object.ModeExec {
		t.Fatal("executable: true was ignored")
	}

	// Without land, nothing moves; kudzu_land lands it later, and again is a no-op.
	out.Land = nil
	ok(t, cs, "kudzu_write", map[string]any{"parent": "main", "message": "docs", "files": []map[string]any{{"path": "docs/a.md", "content": "a\n"}}}, &out)
	if out.Land != nil {
		t.Fatal("landed without land")
	}
	if now, _ := e.repo.Meta.GetRef(ctx, "main"); now != tip {
		t.Fatal("main moved without land")
	}
	var land struct {
		Landed   string `json:"landed"`
		Previous string `json:"previous"`
		Already  bool   `json:"already_landed"`
	}
	ok(t, cs, "kudzu_land", map[string]any{"head": out.Changeset[:12]}, &land)
	if land.Landed != out.Changeset || land.Previous != tip.String() || land.Already {
		t.Fatalf("land = %+v", land)
	}
	ok(t, cs, "kudzu_land", map[string]any{"head": out.Changeset, "ref": "main"}, &land)
	if !land.Already {
		t.Fatalf("second land = %+v", land)
	}

	// Rebase creates new changesets without moving any ref.
	var rb struct{ Head, Onto string }
	ok(t, cs, "kudzu_rebase", map[string]any{"head": base.String(), "onto": "main"}, &rb)
	if rb.Head == "" {
		t.Fatalf("rebase = %+v", rb)
	}

	if msg := fails(t, cs, "kudzu_write", map[string]any{"parent": "main", "message": "empty"}); !strings.Contains(msg, "nothing to write") {
		t.Fatalf("empty write: %s", msg)
	}
	if msg := fails(t, cs, "kudzu_write", map[string]any{"parent": "main", "message": "bad", "files": []map[string]any{{"path": "../x", "content": ""}}}); !strings.Contains(msg, "invalid request") {
		t.Fatalf("bad path: %s", msg)
	}
}

func TestConflicts(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, Options{})
	base, _ := e.repo.Resolve(context.Background(), "main")
	edit := func(content string) map[string]any {
		return map[string]any{
			"parent":  base.String(),
			"message": "edit handler",
			"files": []map[string]any{
				{"path": "services/api/handler.go", "content": content},
				{"path": "README.md", "content": "# " + content},
			},
		}
	}
	first := edit("package main\n\nfunc serve() { println(\"a\") }\n")
	first["land"] = "main"
	ok(t, cs, "kudzu_write", first, nil)

	second := edit("package main\n\nfunc serve() { println(\"b\") }\n")
	second["land"] = "main"
	msg := fails(t, cs, "kudzu_write", second)
	if !strings.Contains(msg, "conflict") || !strings.Contains(msg, "Conflicting paths: README.md, services/api/handler.go") ||
		!strings.Contains(msg, "created changeset") {
		t.Fatalf("conflicting write: %s", msg)
	}

	// The same through kudzu_land and kudzu_rebase.
	var out struct{ Changeset string }
	ok(t, cs, "kudzu_write", edit("package main\n\nfunc serve() { println(\"c\") }\n"), &out)
	msg = fails(t, cs, "kudzu_land", map[string]any{"head": out.Changeset})
	if !strings.Contains(msg, "could not land") || !strings.Contains(msg, "services/api/handler.go") {
		t.Fatalf("conflicting land: %s", msg)
	}
	msg = fails(t, cs, "kudzu_rebase", map[string]any{"head": out.Changeset, "onto": "main"})
	if !strings.Contains(msg, "could not rebase") || !strings.Contains(msg, "README.md") {
		t.Fatalf("conflicting rebase: %s", msg)
	}
}

func TestWorkingCopyTools(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(t, Options{Author: "wc-agent"})
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wc")
	if _, err := client.Clone(ctx, e.url, dir, "main", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st struct {
		Root  string
		Files []struct{ Path, Status string }
	}
	ok(t, cs, "kudzu_wc_status", map[string]any{"dir": filepath.Join(dir, "services")}, &st)
	if st.Root != dir || len(st.Files) != 2 || st.Files[0] != (struct{ Path, Status string }{"README.md", "M"}) || st.Files[1].Status != "?" {
		t.Fatalf("status = %+v", st)
	}

	// Someone else lands meanwhile; committing with land rebases on the server.
	main, _ := e.repo.Resolve(ctx, "main")
	e.commit(t, main, "other", "other", map[string]string{"docs/other.md": "x\n"})
	var commit struct {
		Changeset string
		Changes   int
		NowAt     string `json:"now_at"`
		Land      struct{ Landed string }
	}
	ok(t, cs, "kudzu_wc_commit", map[string]any{"dir": dir, "message": "wc change", "add_untracked": true, "land": "main"}, &commit)
	tip, _ := e.repo.Meta.GetRef(ctx, "main")
	if commit.Changes != 2 || commit.Land.Landed != tip.String() || commit.NowAt != tip.String() {
		t.Fatalf("commit = %+v (main %s)", commit, tip.Short())
	}
	if _, err := os.Stat(filepath.Join(dir, "docs/other.md")); err != nil {
		t.Fatal("working copy did not move to the landed changeset")
	}
	if msg := fails(t, cs, "kudzu_wc_commit", map[string]any{"dir": dir, "message": "again"}); !strings.Contains(msg, "nothing to commit") {
		t.Fatalf("empty commit: %s", msg)
	}

	// Update refuses to clobber local edits, then moves once they are gone.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644)
	if msg := fails(t, cs, "kudzu_wc_update", map[string]any{"dir": dir, "rev": "main~1"}); !strings.Contains(msg, "uncommitted") {
		t.Fatalf("dirty update: %s", msg)
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# changed\n"), 0o644)
	var upd struct{ Previous, Changeset string }
	ok(t, cs, "kudzu_wc_update", map[string]any{"dir": dir, "rev": "main~1"}, &upd)
	if upd.Previous != tip.String() || upd.Changeset == tip.String() {
		t.Fatalf("update = %+v", upd)
	}
	if msg := fails(t, cs, "kudzu_wc_status", map[string]any{"dir": t.TempDir()}); !strings.Contains(msg, "not inside a kudzu working copy") {
		t.Fatalf("status outside a working copy: %s", msg)
	}
}

// TestStdioWire drives the server with raw newline-delimited JSON-RPC, the
// way `kudzu mcp` is used over stdio.
func TestStdioWire(t *testing.T) {
	e := newEnv(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewServer(e.remote, Options{ReadOnly: true}).Run(ctx, &sdk.IOTransport{Reader: inR, Writer: outW})
	}()
	defer func() {
		inW.Close()
		cancel()
		<-done
	}()
	responses := bufio.NewScanner(outR)
	responses.Buffer(make([]byte, 1<<20), 1<<20)
	send := func(msg string) {
		if _, err := io.WriteString(inW, msg+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() map[string]any {
		if !responses.Scan() {
			t.Fatalf("no response: %v", responses.Err())
		}
		var m map[string]any
		if err := json.Unmarshal(responses.Bytes(), &m); err != nil {
			t.Fatalf("bad response %q: %v", responses.Text(), err)
		}
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	init := recv()
	result, _ := init["result"].(map[string]any)
	info, _ := result["serverInfo"].(map[string]any)
	if init["id"] != 1.0 || info["name"] != "kudzu" || result["instructions"] == "" {
		t.Fatalf("initialize = %v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if m := recv(); m["id"] != 2.0 || m["error"] != nil {
		t.Fatalf("ping = %v", m)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kudzu_cat","arguments":{"path":"missing.txt"}}}`)
	m := recv()
	res, _ := m["result"].(map[string]any)
	if m["id"] != 3.0 || res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "not found") {
		t.Fatalf("tool error = %v", m)
	}
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	m = recv()
	res, _ = m["result"].(map[string]any)
	if tools, _ := res["tools"].([]any); len(tools) != 11 {
		t.Fatalf("tools/list returned %d tools", len(tools))
	}
}
