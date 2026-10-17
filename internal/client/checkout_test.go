package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
)

// hostile stands in for a compromised server, cache or daemon: /api/resolve
// names root, and the object protocol serves whatever objects it was given,
// in whatever form. Objects named in last are sent after a pause, so the
// order in which a client writes files is predictable.
type hostile struct {
	objs map[object.ID][]byte
	root object.ID
	last map[object.ID]bool
}

func newHostile() *hostile {
	return &hostile{objs: map[object.ID][]byte{}, last: map[object.ID]bool{}}
}

func (h *hostile) blob(content string) object.ID {
	id, stored := object.EncodeObject(object.KindBlob, []byte(content))
	h.objs[id] = stored
	return id
}

// tree encodes entries as given: unsorted, repeated or reserved names too.
func (h *hostile) tree(es ...object.Entry) object.ID {
	id, stored := object.EncodeObject(object.KindTree, (&object.Tree{Entries: es}).Encode())
	h.objs[id] = stored
	return id
}

func (h *hostile) serve(t *testing.T) string {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/resolve", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]object.ID{"changeset": object.Hash(object.KindChangeset, []byte("x")), "tree": h.root})
	})
	mux.HandleFunc("POST /objects/fetch", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ids, _ := proto.DecodeIDs(b)
		var late []object.ID
		for _, id := range ids {
			if h.last[id] {
				late = append(late, id)
			} else if stored, ok := h.objs[id]; ok {
				proto.WriteFrame(w, id, stored)
			} else {
				proto.WriteMissing(w, id)
			}
		}
		if len(late) > 0 {
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
			for _, id := range late {
				proto.WriteFrame(w, id, h.objs[id])
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func file(name string, id object.ID) object.Entry {
	return object.Entry{Name: name, Mode: object.ModeFile, ID: id}
}

func dir(name string, id object.ID) object.Entry {
	return object.Entry{Name: name, Mode: object.ModeDir, ID: id}
}

// newServer starts a real server; wrap, if set, sees every request first.
func newServer(t *testing.T, wrap func(http.ResponseWriter, *http.Request)) (*repo.Repo, string) {
	meta, err := store.OpenSQLite(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })
	r := repo.New(meta, store.NewMem(64<<20))
	h := server.New(r, log.New(io.Discard, "", 0)).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if wrap != nil {
			wrap(w, req)
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return r, srv.URL
}

// commit creates a changeset on parent (zero for a root) and moves main to
// it. An empty content deletes the path.
func commit(t *testing.T, r *repo.Repo, parent object.ID, files map[string]string) object.ID {
	t.Helper()
	ctx := context.Background()
	var changes []object.Change
	for p, c := range files {
		ch := object.Change{Path: p}
		if c != "" {
			id, err := r.WriteBlob(ctx, []byte(c))
			if err != nil {
				t.Fatal(err)
			}
			ch.Mode, ch.Blob = object.ModeFile, id
		}
		changes = append(changes, ch)
	}
	var parents []object.ID
	if !parent.IsZero() {
		parents = []object.ID{parent}
	}
	c, err := r.Create(ctx, repo.CreateRequest{Parents: parents, Author: "a", Message: "m", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", parent, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func clone(t *testing.T, remote string, sp sparse.Spec) *WC {
	t.Helper()
	wc, err := Clone(context.Background(), remote, filepath.Join(t.TempDir(), "wc"), "main", sp, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return wc
}

func writeTo(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFrom(t *testing.T, root, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	if es, err := os.ReadDir(dir); err != nil || len(es) != 0 {
		t.Fatalf("something was written outside the working copy, in %s: %v %v", dir, es, err)
	}
}

// A nested .git/config can make any git command run in that directory
// execute a program (core.fsmonitor); .kudzu holds the working copy's state.
func TestCloneRefusesReservedNames(t *testing.T) {
	for _, name := range []string{".git", ".GIT", ".git.", ".git ", "git~1", ".kudzu", ".Kudzu"} {
		h := newHostile()
		config := h.blob("[core]\n\tfsmonitor = \"touch /tmp/pwned; false\"\n")
		sub := h.tree(dir(name, h.tree(file("HEAD", h.blob("ref: refs/heads/main\n")), file("config", config))))
		h.root = h.tree(file("README", h.blob("hi\n")), dir("sub", sub))
		dst := filepath.Join(t.TempDir(), "wc")
		_, err := Clone(context.Background(), h.serve(t), dst, "main", nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "sub/"+name) {
			t.Errorf("clone with sub/%s: err = %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dst, "sub", name, "config")); err == nil {
			t.Errorf("clone wrote sub/%s/config", name)
		}
	}
}

// A git checkout nested in the working copy is not part of it: no changeset
// may hold its .git, so status and commit -A leave it out.
func TestStatusIgnoresNestedGit(t *testing.T) {
	r, remote := newServer(t, nil)
	commit(t, r, object.Zero, map[string]string{"README": "hi\n"})
	wc := clone(t, remote, nil)
	writeTo(t, wc.Root, "vendor/lib/.git/HEAD", "ref: refs/heads/main\n")
	writeTo(t, wc.Root, "vendor/lib/lib.go", "package lib\n")
	writeTo(t, wc.Root, "vendor/sub/.git", "gitdir: ../../.git/modules/sub\n")
	ctx := context.Background()
	st, err := wc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Path != "vendor/lib/lib.go" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := wc.Commit(ctx, "vendor lib", "a", true, ""); err != nil {
		t.Fatal(err)
	}
}

// A tree holding both a symlink "a" and a directory "a": the clone made
// directory a, replaced it with the symlink, then wrote a/payload through it.
func TestCloneRefusesRepeatedNames(t *testing.T) {
	outside := t.TempDir()
	h := newHostile()
	payload := h.blob("pwned\n")
	h.last[payload] = true
	h.root = h.tree(
		object.Entry{Name: "a", Mode: object.ModeSymlink, ID: h.blob(outside)},
		dir("a", h.tree(file("payload", payload))),
	)
	if _, err := Clone(context.Background(), h.serve(t), filepath.Join(t.TempDir(), "wc"), "main", nil, io.Discard); err == nil {
		t.Error("clone accepted a tree with a repeated name")
	}
	emptyDir(t, outside)
}

// An untracked symlink where an update needs a directory was followed.
func TestUpdateNeverWritesThroughSymlink(t *testing.T) {
	r, remote := newServer(t, nil)
	c1 := commit(t, r, object.Zero, map[string]string{"README": "hi\n"})
	wc := clone(t, remote, nil)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(wc.Root, "a")); err != nil {
		t.Fatal(err)
	}
	c2 := commit(t, r, c1, map[string]string{"a/payload": "upstream\n"})
	ctx := context.Background()
	var ue *UntrackedError
	if err := wc.Update(ctx, c2.String(), false); !errors.As(err, &ue) || len(ue.Paths) != 1 || ue.Paths[0] != "a" {
		t.Errorf("update through an untracked symlink: %v", err)
	}
	emptyDir(t, outside)
	// --force replaces the symlink with a directory; it never follows it.
	if err := wc.Update(ctx, c2.String(), true); err != nil {
		t.Fatal(err)
	}
	emptyDir(t, outside)
	if info, err := os.Lstat(filepath.Join(wc.Root, "a")); err != nil || !info.IsDir() {
		t.Fatalf("a is not a directory after a forced update: %v", err)
	}
	if got := readFrom(t, wc.Root, "a/payload"); got != "upstream\n" {
		t.Fatalf("a/payload = %q", got)
	}
}

// The checks under apply's own: whatever slipped past them, a write or a
// directory never goes through a symlink.
func TestWriteChecksParents(t *testing.T) {
	r, remote := newServer(t, nil)
	commit(t, r, object.Zero, map[string]string{"README": "hi\n"})
	wc := clone(t, remote, nil)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(wc.Root, "d")); err != nil {
		t.Fatal(err)
	}
	id := object.Hash(object.KindBlob, []byte("x"))
	for _, force := range []bool{false, true} {
		if _, err := wc.writeFile("d/x", object.ModeFile, id, []byte("x"), true, force); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("writeFile(d/x, force=%v) through a symlink: %v", force, err)
		}
		if _, err := wc.writeFile("d/l", object.ModeSymlink, id, []byte("/"), true, force); err == nil {
			t.Errorf("symlink d/l (force=%v) created through a symlink", force)
		}
	}
	if err := wc.mkdirs("d/sub", false); err == nil {
		t.Error("mkdirs followed a symlink")
	}
	emptyDir(t, outside)
	if err := wc.mkdirs("d/sub", true); err != nil {
		t.Fatal(err)
	}
	emptyDir(t, outside)
	if info, err := os.Lstat(filepath.Join(wc.Root, "d", "sub")); err != nil || !info.IsDir() {
		t.Fatalf("forced mkdirs did not replace the symlink: %v", err)
	}
}

// A plain update deleted an untracked directory where upstream added a file,
// and overwrote an untracked file upstream added too.
func TestUpdateKeepsUntrackedFiles(t *testing.T) {
	r, remote := newServer(t, nil)
	c1 := commit(t, r, object.Zero, map[string]string{"README": "hi\n", "gen/old.txt": "old\n", "x": "x\n"})
	wc := clone(t, remote, nil)
	writeTo(t, wc.Root, "build/artifact.bin", "hours of work")
	writeTo(t, wc.Root, "notes.txt", "my notes")
	writeTo(t, wc.Root, "gen/local.txt", "mine")
	writeTo(t, wc.Root, ".kudzuignore", "*.log\n")
	writeTo(t, wc.Root, "debug.log", "ignored, but still mine")
	c2 := commit(t, r, c1, map[string]string{
		"README":      "hello\n",
		"build":       "now a file\n",
		"notes.txt":   "upstream\n",
		"gen/old.txt": "", "gen": "gen is a file now\n",
		"debug.log": "upstream log\n",
	})
	ctx := context.Background()
	err := wc.Update(ctx, c2.String(), false)
	var ue *UntrackedError
	if !errors.As(err, &ue) || strings.Join(ue.Paths, " ") != "build/artifact.bin debug.log gen/local.txt notes.txt" {
		t.Errorf("update over untracked files: %v", err)
	}
	for p, want := range map[string]string{"build/artifact.bin": "hours of work", "notes.txt": "my notes", "gen/local.txt": "mine", "debug.log": "ignored, but still mine"} {
		if b, _ := os.ReadFile(filepath.Join(wc.Root, filepath.FromSlash(p))); string(b) != want {
			t.Errorf("untracked %s = %q", p, b)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	// Nothing changed: not even files that were safe to update.
	if wc.State.Changeset != c1 || readFrom(t, wc.Root, "README") != "hi\n" || readFrom(t, wc.Root, "gen/old.txt") != "old\n" {
		t.Fatal("a refused update changed the working copy")
	}
	// --force discards them.
	if err := wc.Update(ctx, c2.String(), true); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"README": "hello\n", "build": "now a file\n", "notes.txt": "upstream\n", "gen": "gen is a file now\n"} {
		if got := readFrom(t, wc.Root, p); got != want {
			t.Fatalf("after --force, %s = %q", p, got)
		}
	}
	if st, err := wc.Status(ctx); err != nil || len(st) != 1 || st[0].Path != ".kudzuignore" {
		t.Fatalf("status after a forced update: %+v %v", st, err)
	}
}

// Tracked files and directories may change kind; only what is untracked
// stops an update.
func TestUpdateReplacesTrackedPaths(t *testing.T) {
	r, remote := newServer(t, nil)
	c1 := commit(t, r, object.Zero, map[string]string{"a/x": "x\n", "a/y": "y\n", "b": "b\n"})
	wc := clone(t, remote, nil)
	// An untracked empty directory left in a does not hold anything.
	if err := os.MkdirAll(filepath.Join(wc.Root, "a", "empty", "dirs"), 0o755); err != nil {
		t.Fatal(err)
	}
	c2 := commit(t, r, c1, map[string]string{"a/x": "", "a/y": "", "a": "a file\n", "b": "", "b/c": "c\n"})
	ctx := context.Background()
	if err := wc.Update(ctx, c2.String(), false); err != nil {
		t.Fatal(err)
	}
	if readFrom(t, wc.Root, "a") != "a file\n" || readFrom(t, wc.Root, "b/c") != "c\n" {
		t.Fatal("update did not swap files and directories")
	}
	if err := wc.Update(ctx, c1.String(), false); err != nil {
		t.Fatal(err)
	}
	if readFrom(t, wc.Root, "a/x") != "x\n" || readFrom(t, wc.Root, "b") != "b\n" {
		t.Fatal("update did not swap them back")
	}
	if st, err := wc.Status(ctx); err != nil || len(st) != 0 {
		t.Fatalf("status: %+v %v", st, err)
	}
}

// Widening a sparse checkout overwrote untracked files in the new directory.
func TestSetSparseKeepsUntrackedFiles(t *testing.T) {
	r, remote := newServer(t, nil)
	commit(t, r, object.Zero, map[string]string{"a/x": "x\n", "b/notes.txt": "upstream\n", "b/y": "y\n"})
	wc := clone(t, remote, sparse.Spec{"a"})
	writeTo(t, wc.Root, "b/notes.txt", "my notes")
	ctx := context.Background()
	var ue *UntrackedError
	if err := wc.SetSparse(ctx, sparse.Spec{"a", "b"}); !errors.As(err, &ue) || len(ue.Paths) != 1 || ue.Paths[0] != "b/notes.txt" {
		t.Fatalf("sparse add over an untracked file: %v", err)
	}
	if got := readFrom(t, wc.Root, "b/notes.txt"); got != "my notes" {
		t.Fatalf("b/notes.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(wc.Root, "b", "y")); err == nil || len(wc.State.Sparse) != 1 {
		t.Fatal("a refused sparse change changed the working copy")
	}
	os.Remove(filepath.Join(wc.Root, "b", "notes.txt"))
	if err := wc.SetSparse(ctx, sparse.Spec{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := readFrom(t, wc.Root, "b/notes.txt"); got != "upstream\n" {
		t.Fatalf("b/notes.txt = %q", got)
	}
}

// Commit used to stat files after the server round trip, so an edit made
// while the commit was in flight was recorded as clean and never committed.
func TestCommitKeepsEditMadeDuringCommit(t *testing.T) {
	var editRoot atomic.Value // the working copy to edit while a commit is in flight
	editRoot.Store("")
	r, remote := newServer(t, func(w http.ResponseWriter, req *http.Request) {
		if root := editRoot.Load().(string); root != "" && req.Method == "POST" && req.URL.Path == "/api/changesets" {
			time.Sleep(20 * time.Millisecond) // a new mtime, even on coarse clocks
			writeTo(t, root, "f.txt", "v3, edited during the commit\n")
		}
	})
	commit(t, r, object.Zero, map[string]string{"f.txt": "v1\n"})
	wc := clone(t, remote, nil)
	writeTo(t, wc.Root, "f.txt", "v2\n")
	ctx := context.Background()
	editRoot.Store(wc.Root)
	if _, err := wc.Commit(ctx, "m", "a", false, ""); err != nil {
		t.Fatal(err)
	}
	editRoot.Store("")
	st, err := wc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Path != "f.txt" || st[0].Code != 'M' {
		t.Fatalf("the edit made during the commit is not in status: %+v", st)
	}
	res, err := wc.Commit(ctx, "m2", "a", false, "")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.ReadTree(ctx, res.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := tree.Lookup("f.txt"); e.ID != object.Hash(object.KindBlob, []byte("v3, edited during the commit\n")) {
		t.Fatal("the next commit does not hold the edit")
	}
}
