package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	if fi, err := os.Stat(sock); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("daemon socket mode %v, want 0600 (only this user may connect)", fi.Mode().Perm())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("up returned %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket not removed on shutdown")
	}
}

// TestDaemonPermissions starts a node daemon with the default socket mode and
// object directory, then with --socket-mode 0660.
func TestDaemonPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permissions")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	if dir := daemonDir(); !strings.HasPrefix(dir, home) {
		t.Fatalf("default daemon directory %s is not under the user's home %s", dir, home)
	}
	sockDir, err := os.MkdirTemp("", "kz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "d.sock")

	for _, tc := range []struct {
		args []string
		want os.FileMode
	}{{nil, 0o600}, {[]string{"--socket-mode", "0660"}, 0o660}} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- cmdDaemon(ctx, append([]string{"--listen", "unix://" + sock, "--upstream", "127.0.0.1:1"}, tc.args...))
		}()
		rem, _ := proto.Dial("unix://" + sock)
		for deadline := time.Now().Add(20 * time.Second); ; {
			if _, err := rem.GetRaw(context.Background(), "/healthz", nil); err == nil {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("daemon never became ready: %v", <-done)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if fi, err := os.Stat(sock); err != nil {
			t.Error(err)
		} else if fi.Mode().Perm() != tc.want {
			t.Errorf("daemon %v: socket mode %v, want %v", tc.args, fi.Mode().Perm(), tc.want)
		}
		if fi, err := os.Stat(daemonDir()); err != nil {
			t.Error(err)
		} else if fi.Mode().Perm() != 0o700 {
			t.Errorf("daemon object directory mode %v, want %v", fi.Mode().Perm(), os.FileMode(0o700))
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var m socketMode
	for _, bad := range []string{"rw", "0o666", "1000", "-1"} {
		if err := m.Set(bad); err == nil {
			t.Errorf("--socket-mode %s was accepted as %v", bad, m.String())
		}
	}
}
