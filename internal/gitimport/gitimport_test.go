package gitimport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
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

func commitAll(t *testing.T, dir, msg string) {
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
}

// buildGitRepo makes a history with edits, deletes, an exec bit, a symlink,
// a dir<->file swap, a rename and a merge.
func buildGitRepo(t *testing.T) string {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "README", "hello\n", 0o644)
	writeFile(t, dir, "src/a.txt", "one\ntwo\nthree\n", 0o644)
	writeFile(t, dir, "src/lib/b.txt", "b\n", 0o644)
	commitAll(t, dir, "initial")

	writeFile(t, dir, "src/a.txt", "one\nTWO\nthree\nfour\n", 0o644)
	writeFile(t, dir, "run.sh", "#!/bin/sh\n", 0o755)
	os.Symlink("src/a.txt", filepath.Join(dir, "link"))
	commitAll(t, dir, "edit, exec, symlink")

	os.RemoveAll(filepath.Join(dir, "src/lib"))
	writeFile(t, dir, "src/lib", "now a file\n", 0o644)
	commitAll(t, dir, "dir becomes file")

	gitCmd(t, dir, "checkout", "-q", "-b", "feature")
	gitCmd(t, dir, "mv", "README", "README.md")
	writeFile(t, dir, "feature/x.txt", "x\n", 0o644)
	commitAll(t, dir, "feature work")

	gitCmd(t, dir, "checkout", "-q", "main")
	writeFile(t, dir, "src/a.txt", "zero\none\nTWO\nthree\nfour\n", 0o644)
	commitAll(t, dir, "main work")
	gitCmd(t, dir, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")

	os.Remove(filepath.Join(dir, "src/lib"))
	writeFile(t, dir, "src/lib/c.txt", "c\n", 0o644)
	commitAll(t, dir, "file becomes dir")
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

// gitSnapshot lists path -> "mode content-hash" for a commit, as git sees it.
func gitSnapshot(t *testing.T, dir, commit string) map[string]string {
	out := map[string]string{}
	for _, rec := range strings.Split(gitCmd(t, dir, "ls-tree", "-r", "-z", commit), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, _ := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		cmd := exec.Command("git", "-C", dir, "cat-file", "blob", f[2])
		content, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		mode := f[0]
		out[path] = mode + " " + object.Hash(object.KindBlob, content).String()
	}
	return out
}

func kudzuSnapshot(t *testing.T, r *repo.Repo, cs object.ID) map[string]string {
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

func TestImportMatchesGit(t *testing.T) {
	dir := buildGitRepo(t)
	r := newRepo(t)
	ctx := context.Background()
	res, err := Run(ctx, r, dir, "main", "main", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 7 {
		t.Fatalf("imported %d commits, want 7", res.Imported)
	}
	for _, sha := range strings.Fields(gitCmd(t, dir, "rev-list", "main")) {
		v, err := r.Meta.GetMapping(ctx, "git", sha)
		if err != nil {
			t.Fatalf("commit %s not mapped: %v", sha, err)
		}
		cs, _ := object.ParseID(v)
		want, got := gitSnapshot(t, dir, sha), kudzuSnapshot(t, r, cs)
		if fmt.Sprint(want) != fmt.Sprint(got) {
			t.Fatalf("commit %s differs\ngit:   %v\nkudzu: %v", sha[:8], want, got)
		}
	}

	// The merge keeps both parents; its changes are relative to the first.
	tip, _ := r.Meta.GetRef(ctx, "main")
	mergeID, err := r.NthAncestor(ctx, tip, 1)
	if err != nil {
		t.Fatal(err)
	}
	merge, _ := r.Changeset(ctx, mergeID)
	if len(merge.Parents) != 2 || merge.Message != "merge feature" {
		t.Fatalf("merge changeset = %+v", merge)
	}

	// First-parent blame matches git's.
	lines, err := r.Blame(ctx, tip, "src/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	porcelain := gitCmd(t, dir, "blame", "--first-parent", "-l", "-s", "main", "--", "src/a.txt")
	gitLines := strings.Split(strings.TrimSpace(porcelain), "\n")
	if len(gitLines) != len(lines) {
		t.Fatalf("blame line counts differ: git %d, kudzu %d", len(gitLines), len(lines))
	}
	for i, gl := range gitLines {
		sha := strings.TrimPrefix(strings.Fields(gl)[0], "^")
		c, _ := r.Changeset(ctx, lines[i].Changeset)
		if !strings.HasPrefix(c.Extra["git"], sha) {
			t.Fatalf("line %d: git blames %s, kudzu blames %s", i+1, sha[:8], c.Extra["git"][:8])
		}
	}

	// Re-importing after a new commit only ingests the new commit.
	writeFile(t, dir, "new.txt", "new\n", 0o644)
	commitAll(t, dir, "one more")
	res2, err := Run(ctx, r, dir, "main", "main", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Imported != 1 || res2.Commits != 8 {
		t.Fatalf("incremental import: %+v", res2)
	}
	tip2, _ := r.Meta.GetRef(ctx, "main")
	if !bytes.Equal(tip2[:], res2.Tip[:]) {
		t.Fatal("ref not advanced")
	}
}

// TestReimportKeepsLandedWork re-runs the import, as `kudzu up` does on every
// start, after a change landed through Kudzu: the ref must not move back to
// the git tip unless that is a fast-forward or forced.
func TestReimportKeepsLandedWork(t *testing.T) {
	dir := buildGitRepo(t)
	r := newRepo(t)
	ctx := context.Background()
	main := func() object.ID {
		id, err := r.Meta.GetRef(ctx, "main")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, err := Run(ctx, r, dir, "main", "main", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RefMoved || first.Kept || first.Ref != first.Tip || main() != first.Tip {
		t.Fatalf("first import = %+v", first)
	}

	blob, _ := r.WriteBlob(ctx, []byte("landed\n"))
	landed, err := r.Create(ctx, repo.CreateRequest{Parents: []object.ID{first.Tip}, Author: "agent", Message: "land through kudzu",
		Changes: []object.Change{{Path: "VERSION", Mode: object.ModeFile, Blob: blob}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Land(ctx, landed.ID, "main"); err != nil {
		t.Fatal(err)
	}

	// Nothing new in git: main keeps the landed changeset.
	again, err := Run(ctx, r, dir, "main", "main", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := main(); got != landed.ID {
		t.Fatalf("re-import moved main from the landed %s back to %s", landed.ID.Short(), got.Short())
	}
	if again.RefMoved || !again.Kept || again.Diverged || again.Ref != landed.ID || again.Tip != first.Tip {
		t.Fatalf("re-import = %+v", again)
	}

	// New git commits that lack the landed change: the two have diverged.
	writeFile(t, dir, "git.txt", "git\n", 0o644)
	commitAll(t, dir, "git work")
	div, err := Run(ctx, r, dir, "main", "main", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := main(); got != landed.ID || div.Imported != 1 || div.RefMoved || !div.Kept || !div.Diverged || div.Ref != landed.ID {
		t.Fatalf("diverged re-import = %+v, main at %s", div, got.Short())
	}

	// Forcing moves it anyway.
	forced, err := Run(ctx, r, dir, "main", "main", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := main(); got != div.Tip || !forced.RefMoved || forced.Kept || forced.Imported != 0 {
		t.Fatalf("forced re-import = %+v, main at %s", forced, got.Short())
	}

	// A fast-forward needs no force.
	writeFile(t, dir, "more.txt", "more\n", 0o644)
	commitAll(t, dir, "more git work")
	ff, err := Run(ctx, r, dir, "main", "main", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := main(); got != ff.Tip || !ff.RefMoved || ff.Kept || ff.Imported != 1 {
		t.Fatalf("fast-forward import = %+v, main at %s", ff, got.Short())
	}
}
