package gitexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/gitimport"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00+08:00", "GIT_COMMITTER_DATE=2026-01-01T00:00:00+08:00",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, p, content string, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(dir, p)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(full, mode)
}

// sourceRepo is a small git history with a merge, an exec bit and a symlink.
func sourceRepo(t *testing.T) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "README", "hello\n", 0o644)
	writeFile(t, dir, "src/a.txt", "one\ntwo\n", 0o644)
	writeFile(t, dir, "src/lib/b.txt", "b\n", 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "initial")
	writeFile(t, dir, "run.sh", "#!/bin/sh\n", 0o755)
	os.Symlink("src/a.txt", filepath.Join(dir, "link"))
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "exec and symlink")
	git(t, dir, "checkout", "-q", "-b", "side")
	writeFile(t, dir, "side.txt", "side\n", 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "side work")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	return dir
}

func newRepo(t *testing.T) *repo.Repo {
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
	return repo.New(meta, &store.Cached{Mem: store.NewMem(32 << 20), Backing: objs})
}

// land creates a changeset on top of main through the server-side API and
// moves main to it.
func land(t *testing.T, r *repo.Repo, author, msg string, set map[string]string, mode uint32, del ...string) object.ID {
	t.Helper()
	ctx := context.Background()
	tip, err := r.Meta.GetRef(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	var changes []object.Change
	for p, content := range set {
		id, _ := r.WriteBlob(ctx, []byte(content))
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	for _, p := range del {
		changes = append(changes, object.Change{Path: p})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Parents: []object.ID{tip}, Author: author, Time: 1767196800, TZ: -300, Message: msg, Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Land(ctx, c.ID, "main"); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// gitTree lists path -> "mode blob-content-hash" for a git commit.
func gitTree(t *testing.T, dir, commit string) map[string]string {
	out := map[string]string{}
	for _, rec := range strings.Split(git(t, dir, "ls-tree", "-r", "-z", commit), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		content, err := exec.Command("git", "-C", dir, "cat-file", "blob", f[2]).Output()
		if err != nil {
			t.Fatal(err)
		}
		out[path] = f[0] + " " + object.Hash(object.KindBlob, content).String()
	}
	return out
}

func kudzuTree(t *testing.T, r *repo.Repo, cs object.ID) map[string]string {
	ctx := context.Background()
	tree, err := r.TreeOf(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	r.WalkTree(ctx, tree, "", func(p string, e object.Entry) error {
		out[p] = fmt.Sprintf("%06o %s", e.Mode, e.ID)
		return nil
	})
	return out
}

// checkMirror verifies that every changeset on main has a git commit in dir
// with the same tree, author and message.
func checkMirror(t *testing.T, r *repo.Repo, dir string) {
	t.Helper()
	ctx := context.Background()
	tip, _ := r.Meta.GetRef(ctx, "main")
	log, err := r.Log(ctx, tip, 1000)
	if err != nil {
		t.Fatal(err)
	}
	gitLog := strings.Split(strings.TrimSpace(git(t, dir, "log", "--first-parent", "--format=%H", "main")), "\n")
	if len(gitLog) != len(log) {
		t.Fatalf("git main has %d first-parent commits, kudzu main has %d", len(gitLog), len(log))
	}
	for i, id := range log {
		sha := gitLog[i]
		if want, got := kudzuTree(t, r, id), gitTree(t, dir, sha); fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("commit %s (changeset %s) differs\nkudzu: %v\ngit:   %v", sha[:8], id.Short(), want, got)
		}
		c, _ := r.Changeset(ctx, id)
		msg := strings.TrimRight(git(t, dir, "log", "-1", "--format=%B", sha), "\n")
		if msg != c.Message {
			t.Fatalf("commit %s message %q, want %q", sha[:8], msg, c.Message)
		}
		if c.Extra["git"] == "" { // created in Kudzu
			want := identity(c.Author, c.Time, c.TZ)
			if who := strings.TrimSpace(git(t, dir, "log", "-1", "--date=format:%z", "--format=%an <%ae> %at %ad", sha)); who != want {
				t.Fatalf("commit %s author %q, want %q", sha[:8], who, want)
			}
		}
	}
}

func TestExportRoundTrip(t *testing.T) {
	src := sourceRepo(t)
	r := newRepo(t)
	ctx := context.Background()
	if _, err := gitimport.Run(ctx, r, src, "main", "main", nil); err != nil {
		t.Fatal(err)
	}
	importedTip := strings.TrimSpace(git(t, src, "rev-parse", "main"))
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	git(t, src, "clone", "-q", "--bare", src, mirror)

	// Work lands in Kudzu: edits, a file that becomes a directory, a
	// directory that becomes a file, odd paths and a merge made on the server.
	land(t, r, "Alice <alice@example.com>", "api: tweak", map[string]string{"src/a.txt": "one\nTWO\n", "new dir/ünï \"q\".txt": "x\n"}, object.ModeFile)
	land(t, r, "Bob <bob@example.com>", "README becomes a directory\n\nWith a body.", map[string]string{"README/index.md": "hi\n"}, object.ModeFile, "README")
	land(t, r, "carol", "lib becomes a file", map[string]string{"src/lib": "now a file\n"}, object.ModeExec, "src/lib/b.txt")
	tip, _ := r.Meta.GetRef(ctx, "main")
	branch := land(t, r, "Dan <dan@example.com>", "on a branch", map[string]string{"branch.txt": "b\n"}, object.ModeFile)
	if err := r.Meta.CASRef(ctx, "main", branch, tip); err != nil { // keep it off main
		t.Fatal(err)
	}
	land(t, r, "Erin <erin@example.com>", "main moves on", map[string]string{"main.txt": "m\n"}, object.ModeFile)
	main, _ := r.Meta.GetRef(ctx, "main")
	merged, err := r.Merge(ctx, main, branch, "Frank <frank@example.com>", "merge branch")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", main, merged.ID); err != nil {
		t.Fatal(err)
	}

	res, err := Run(ctx, r, Options{GitDir: mirror, Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exported != 6 || res.Previous != importedTip || res.Branch != "refs/heads/main" {
		t.Fatalf("export = %+v", res)
	}
	checkMirror(t, r, mirror)
	if out := git(t, mirror, "log", "-1", "--format=%P", "main"); len(strings.Fields(out)) != 2 {
		t.Fatalf("merge commit has parents %q", out)
	}
	if head := strings.TrimSpace(git(t, mirror, "log", "-1", "--format=%H", "main^2~4")); head != importedTip {
		t.Fatalf("exported history does not continue the imported commits: %s", head)
	}
	if out := git(t, mirror, "for-each-ref", "refs/kudzu-export"); out != "" {
		t.Fatalf("scratch ref left behind: %s", out)
	}

	// Exporting again is a no-op, and importing the mirror creates nothing.
	again, err := Run(ctx, r, Options{GitDir: mirror, Ref: "main"})
	if err != nil || again.Exported != 0 || again.Tip != res.Tip {
		t.Fatalf("second export = %+v, %v", again, err)
	}
	back, err := gitimport.Run(ctx, r, mirror, "main", "main", nil)
	if err != nil || back.Imported != 0 || back.Tip != merged.ID {
		t.Fatalf("re-import of the mirror = %+v, %v", back, err)
	}

	// Landing more exports only the new changeset.
	land(t, r, "Alice <alice@example.com>", "one more", map[string]string{"src/a.txt": "three\n"}, object.ModeFile)
	inc, err := Run(ctx, r, Options{GitDir: mirror, Ref: "main"})
	if err != nil || inc.Exported != 1 || inc.Previous != res.Tip {
		t.Fatalf("incremental export = %+v, %v", inc, err)
	}
	checkMirror(t, r, mirror)

	// A commit pushed to git that Kudzu lacks blocks the export unless forced.
	git(t, mirror, "update-ref", "refs/heads/main", strings.TrimSpace(git(t, mirror, "commit-tree", "-p", "main", "-m", "git only", "main^{tree}")))
	land(t, r, "Alice <alice@example.com>", "after the git-only commit", map[string]string{"x": "x\n"}, object.ModeFile)
	if _, err := Run(ctx, r, Options{GitDir: mirror, Ref: "main"}); !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("expected a non-fast-forward error, got %v", err)
	}
	if _, err := Run(ctx, r, Options{GitDir: mirror, Ref: "main", Force: true}); err != nil {
		t.Fatal(err)
	}
	checkMirror(t, r, mirror)
}

// TestExportToEmptyRepo writes the whole history, imported commits included,
// into a repository that has none of it.
func TestExportToEmptyRepo(t *testing.T) {
	src := sourceRepo(t)
	r := newRepo(t)
	ctx := context.Background()
	if _, err := gitimport.Run(ctx, r, src, "main", "main", nil); err != nil {
		t.Fatal(err)
	}
	land(t, r, "Alice <alice@example.com>", "kudzu work", map[string]string{"k.txt": "k\n"}, object.ModeFile)
	empty := filepath.Join(t.TempDir(), "empty.git")
	git(t, t.TempDir(), "init", "-q", "--bare", empty)
	res, err := Run(ctx, r, Options{GitDir: empty, Ref: "main", Branch: "trunk"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exported != 5 || res.Previous != "" || res.Branch != "refs/heads/trunk" {
		t.Fatalf("export = %+v", res)
	}
	git(t, empty, "branch", "-q", "-m", "trunk", "main")
	checkMirror(t, r, empty)
	if _, err := Run(ctx, r, Options{GitDir: t.TempDir(), Ref: "main"}); err == nil {
		t.Fatal("exporting into a directory that is not a git repository should fail")
	}
}

func TestIdentity(t *testing.T) {
	for in, want := range map[string]string{
		"Alice <a@example.com>": "Alice <a@example.com> 100 +0800",
		"bob":                   "bob <> 100 +0800",
		"<x@y>":                 "<x@y> 100 +0800",
		"Eve <<evil>>":          "Eve evil <> 100 +0800",
	} {
		if got := identity(in, 100, 480); got != want {
			t.Errorf("identity(%q) = %q, want %q", in, got, want)
		}
	}
	if got := identity("a <b>", 0, -90); got != "a <b> 0 -0130" {
		t.Errorf("negative tz: %q", got)
	}
}
