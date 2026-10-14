package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/proto"
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
