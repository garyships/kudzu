package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
