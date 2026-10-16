package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
)

// TestUp runs the one-command playground against a fresh git repository and
// queries it through the node daemon's unix socket.
func TestUp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "empty"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(repoDir, "hello.txt"), []byte("hi\n"), 0o644)
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "add hello"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	sockDir, err := os.MkdirTemp("", "kz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "up.sock")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- cmdUp(ctx, []string{"--git", repoDir, "--dir", t.TempDir(), "--port", "0", "--socket", sock})
	}()
	rem, _ := proto.Dial("unix://" + sock)
	var log []struct {
		Message string `json:"message"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := rem.GetJSON(context.Background(), "/api/log", url.Values{"rev": {"main"}}, &log); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("kudzu up never became ready: %v", <-done)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(log) != 2 || log[0].Message != "add hello" {
		t.Fatalf("log through the daemon = %+v", log)
	}
	b, err := rem.GetRaw(context.Background(), "/api/cat", url.Values{"rev": {"main"}, "path": {"hello.txt"}})
	if err != nil || string(b) != "hi\n" {
		t.Fatalf("cat = %q, %v", b, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("up returned %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket not removed on shutdown")
	}
}

// TestImportKeepsLandedWork re-imports after a change landed in kudzu: main
// stays on it, a warning says how to reconcile, and --force moves it anyway.
func TestImportKeepsLandedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	gitDir, storeDir := t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "empty"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", gitDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(t, cmdImport, "--store", storeDir, "--git", gitDir)

	ctx := context.Background()
	r, closeRepo, err := openRepo(storeDir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := r.Meta.GetRef(ctx, "main")
	blob, _ := r.WriteBlob(ctx, []byte("landed\n"))
	landed, err := r.Create(ctx, repo.CreateRequest{Parents: []object.ID{tip}, Message: "landed in kudzu",
		Changes: []object.Change{{Path: "VERSION", Mode: object.ModeFile, Blob: blob}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Land(ctx, landed.ID, "main"); err != nil {
		t.Fatal(err)
	}
	closeRepo()
	mainRef := func() object.ID {
		r, closeRepo, err := openRepo(storeDir, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer closeRepo()
		id, _ := r.Meta.GetRef(ctx, "main")
		return id
	}

	stderr := filepath.Join(t.TempDir(), "stderr")
	f, err := os.Create(stderr)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = f
	out := run(t, cmdImport, "--store", storeDir, "--git", gitDir)
	os.Stderr = saved
	f.Close()
	warning, _ := os.ReadFile(stderr)
	if got := mainRef(); got != landed.ID || !strings.Contains(out, "main -> "+landed.ID.Short()) {
		t.Fatalf("re-import moved main to %s:\n%s", got.Short(), out)
	}
	for _, want := range []string{"warning: left main at " + landed.ID.Short() + ": it has changesets that HEAD (imported as " + tip.Short() + ") lacks", "kudzu export --store " + storeDir + " --git " + gitDir + " --ref main --as kudzu-main", "--force"} {
		if !strings.Contains(string(warning), want) {
			t.Fatalf("warning lacks %q:\n%s", want, warning)
		}
	}

	run(t, cmdImport, "--store", storeDir, "--git", gitDir, "--force")
	if got := mainRef(); got != tip {
		t.Fatalf("import --force left main at %s, want the git tip %s", got.Short(), tip.Short())
	}
}
