package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
)

// startServer runs an authoritative server with one changeset on main and
// points $KUDZU_REMOTE at it.
func startServer(t *testing.T) *repo.Repo {
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
	r := repo.New(meta, &store.Cached{Mem: store.NewMem(16 << 20), Backing: objs})
	srv := httptest.NewServer(server.New(r, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	ctx := context.Background()
	blob, _ := r.WriteBlob(ctx, []byte("hello\n"))
	c, err := r.Create(ctx, repo.CreateRequest{Author: "seed <seed@example.com>", Message: "initial",
		Changes: []object.Change{{Path: "docs/hello.txt", Mode: object.ModeFile, Blob: blob}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUDZU_REMOTE", srv.URL)
	t.Setenv("KUDZU_AUTHOR", "Tester <t@example.com>")
	t.Chdir(t.TempDir()) // not inside a working copy
	return r
}

// run calls a command and returns what it printed on stdout.
func run(t *testing.T, cmd func(context.Context, []string) error, args ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	err = cmd(context.Background(), args)
	os.Stdout = stdout
	w.Close()
	s := <-out
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return s
}

func decode(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
}

func TestJSONOutput(t *testing.T) {
	r := startServer(t)
	ctx := context.Background()
	if out := run(t, cmdLog, "--oneline"); !strings.HasSuffix(out, " initial\n") {
		t.Fatalf("human log changed: %q", out)
	}

	var log []struct{ ID, Message, Author string }
	decode(t, run(t, cmdLog, "--json", "-n", "5"), &log)
	if len(log) != 1 || log[0].Message != "initial" {
		t.Fatalf("log --json = %+v", log)
	}

	note := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(note, []byte("note\n"), 0o644)
	var w struct {
		ID       string
		Files    int
		Uploaded int
		Land     struct{ Ref, Landed string }
	}
	decode(t, run(t, cmdWrite, "--json", "-m", "add note", "--land", "main", "notes/a.txt=@"+note), &w)
	if w.Files != 1 || w.Uploaded != 1 || w.Land.Ref != "main" || w.Land.Landed != w.ID {
		t.Fatalf("write --json = %+v", w)
	}

	var refs map[string]string
	decode(t, run(t, cmdRefs, "--json"), &refs)
	if refs["main"] != w.ID {
		t.Fatalf("refs --json = %v", refs)
	}
	var show struct {
		ID      string
		Changes []struct{ Path string }
	}
	decode(t, run(t, cmdShow, "--json", "main"), &show)
	if show.ID != w.ID || len(show.Changes) != 1 || show.Changes[0].Path != "notes/a.txt" {
		t.Fatalf("show --json = %+v", show)
	}
	var diff struct {
		Files []struct{ Path string }
		Patch string
	}
	decode(t, run(t, cmdDiff, "--json", "--to", "main"), &diff)
	if len(diff.Files) != 1 || !strings.Contains(diff.Patch, "+note") {
		t.Fatalf("diff --json = %+v", diff)
	}
	var ls []struct{ Name string }
	decode(t, run(t, cmdLs, "--json", "notes"), &ls)
	if len(ls) != 1 || ls[0].Name != "a.txt" {
		t.Fatalf("ls --json = %+v", ls)
	}
	var blame []struct {
		Line   int
		Author string
	}
	decode(t, run(t, cmdBlame, "--json", "docs/hello.txt"), &blame)
	if len(blame) != 1 || blame[0].Author != "seed <seed@example.com>" {
		t.Fatalf("blame --json = %+v", blame)
	}
	var mb struct {
		MergeBase   string `json:"merge_base"`
		AIsAncestor bool   `json:"a_is_ancestor_of_b"`
	}
	decode(t, run(t, cmdMergeBase, "--json", "main~1", "main"), &mb)
	if mb.MergeBase != log[0].ID || !mb.AIsAncestor {
		t.Fatalf("merge-base --json = %+v", mb)
	}

	// Working copy commands.
	wcDir := filepath.Join(t.TempDir(), "wc")
	run(t, cmdClone, os.Getenv("KUDZU_REMOTE"), wcDir)
	t.Chdir(wcDir)
	os.WriteFile("docs/hello.txt", []byte("hello again\n"), 0o644)
	var st struct {
		Changeset string
		Files     []struct{ Path, Status string }
	}
	decode(t, run(t, cmdStatus, "--json"), &st)
	if st.Changeset != w.ID || len(st.Files) != 1 || st.Files[0].Status != "M" {
		t.Fatalf("status --json = %+v", st)
	}
	decode(t, run(t, cmdDiff, "--json"), &diff)
	if len(diff.Files) != 1 || !strings.Contains(diff.Patch, "+hello again") {
		t.Fatalf("local diff --json = %+v", diff)
	}
	var commit struct {
		ID      string
		Changes int
		Land    *struct{ Ref string }
	}
	decode(t, run(t, cmdCommit, "--json", "-m", "edit"), &commit)
	if commit.Changes != 1 || commit.Land != nil {
		t.Fatalf("commit --json = %+v", commit)
	}
	var land struct {
		Ref, Landed string
		Rebased     bool
	}
	decode(t, run(t, cmdLand, "--json"), &land)
	if land.Ref != "main" || land.Landed != commit.ID || land.Rebased {
		t.Fatalf("land --json = %+v", land)
	}
	if tip, _ := r.Meta.GetRef(ctx, "main"); tip.String() != commit.ID {
		t.Fatal("main did not move")
	}
	if out := run(t, cmdStatus); out != "" {
		t.Fatalf("human status after land = %q", out)
	}
}

// TestMCPCommand runs `kudzu mcp --read-only` on a fake stdin/stdout.
func TestMCPCommand(t *testing.T) {
	startServer(t)
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	done := make(chan error, 1)
	go func() { done <- cmdMCP(context.Background(), []string{"--read-only"}) }()
	t.Cleanup(func() { os.Stdin, os.Stdout = stdin, stdout })

	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"kudzu_cat","arguments":{"path":"docs/hello.txt"}}}`+"\n")
	sc := bufio.NewScanner(outR)
	var got []string
	for len(got) < 2 && sc.Scan() {
		got = append(got, sc.Text())
	}
	inW.Close() // EOF ends the session
	if err := <-done; err != nil {
		t.Fatalf("kudzu mcp: %v", err)
	}
	outW.Close()
	if len(got) != 2 || !strings.Contains(got[0], `"name":"kudzu"`) || !strings.Contains(got[1], `hello\n`) {
		t.Fatalf("responses = %q", got)
	}
}
