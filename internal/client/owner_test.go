package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
)

// TestOpenRefusesForeignMetadata: a .kudzu directory that belongs to
// another user, e.g. planted in /tmp, is not used.
func TestOpenRefusesForeignMetadata(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing a directory's owner needs root")
	}
	root := t.TempDir()
	meta := filepath.Join(root, metaDir)
	if err := os.Mkdir(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(meta, 12345, 12345); err != nil {
		t.Skip(err)
	}
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	t.Setenv("SUDO_UID", "")
	t.Setenv("KUDZU_SAFE_DIRECTORY", "")
	_, err := Open(sub)
	if err == nil || !strings.Contains(err.Error(), "belongs to user 12345") {
		t.Fatalf("Open = %v", err)
	}
	// Trusting it explicitly, or being root under sudo for that user, gets
	// past the check (and then fails only because there is no state).
	for _, env := range [][2]string{{"KUDZU_SAFE_DIRECTORY", root}, {"KUDZU_SAFE_DIRECTORY", "*"}, {"SUDO_UID", "12345"}} {
		t.Setenv(env[0], env[1])
		if _, err := Open(sub); err == nil || strings.Contains(err.Error(), "belongs to user") {
			t.Fatalf("with %s=%s: %v", env[0], env[1], err)
		}
		t.Setenv(env[0], "")
	}
}

// TestOpenRefusesRedirectableMetadata: Open uses a .kudzu only if it is a
// real directory that nobody else can write to. Through a symlink, or with
// others able to replace what is in it, the state that names the remote
// could be someone else's even though the directory checks out as the
// user's own.
func TestOpenRefusesRedirectableMetadata(t *testing.T) {
	r, remote := newServer(t, nil)
	commit(t, r, object.ID{}, map[string]string{"README.md": "hi\n", "src/a.go": "package a\n"})
	wc := clone(t, remote, nil)
	meta := filepath.Join(wc.Root, metaDir)
	sub := filepath.Join(wc.Root, "src")
	t.Setenv("SUDO_UID", "")
	t.Setenv("KUDZU_SAFE_DIRECTORY", "")
	if _, err := Open(sub); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []os.FileMode{0o775, 0o757, 0o777 | os.ModeSticky} {
		if err := os.Chmod(meta, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(sub); err == nil || !strings.Contains(err.Error(), "chmod go-w "+meta) {
			t.Fatalf(".kudzu with mode %s: Open = %v", mode, err)
		}
		t.Setenv("KUDZU_SAFE_DIRECTORY", wc.Root)
		if _, err := Open(sub); err != nil {
			t.Fatalf(".kudzu with mode %s, trusted: %v", mode, err)
		}
		t.Setenv("KUDZU_SAFE_DIRECTORY", "")
	}
	if err := os.Chmod(meta, 0o755); err != nil {
		t.Fatal(err)
	}

	// The user's own metadata, reached through a link: a link in a shared
	// directory can be pointed elsewhere between the check and the reads.
	elsewhere := filepath.Join(t.TempDir(), "meta")
	if err := os.Rename(meta, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, meta); err != nil {
		t.Skip(err)
	}
	if _, err := Open(sub); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked .kudzu: Open = %v", err)
	}
}
