//go:build unix

package proto

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSocketOwnerCheck serves on a socket in a sticky, world-writable
// directory like /tmp and pretends different users own it.
func TestSocketOwnerCheck(t *testing.T) {
	// Unix socket paths are length-limited; keep it short.
	dir, err := os.MkdirTemp("", "kzs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(l)
	defer srv.Close()
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}

	owner := os.Getuid()
	saved := socketOwner
	socketOwner = func(os.FileInfo) (int, bool) { return owner, true }
	t.Cleanup(func() { socketOwner = saved })
	get := func() error {
		t.Helper()
		r, err := Dial("unix://" + sock)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.GetRaw(context.Background(), "/healthz", nil)
		return err
	}

	if err := get(); err != nil {
		t.Fatalf("socket of the current user: %v", err)
	}
	owner = 0
	if err := get(); err != nil {
		t.Fatalf("socket of root: %v", err)
	}
	owner = os.Getuid() + 4242
	err = get()
	if err == nil {
		t.Fatal("a socket another user owns in a world-writable directory was accepted")
	}
	for _, want := range []string{sock, "uid " + strconv.Itoa(owner), "world-writable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// The same socket in a directory only its owner can write to is fine.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := get(); err != nil {
		t.Fatalf("socket in a private directory: %v", err)
	}

	// A link from a world-writable directory is checked as well.
	pub, err := os.MkdirTemp("", "kzs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(pub)
	if err := os.Chmod(pub, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pub, "l.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	r, _ := Dial("unix://" + link)
	if _, err := r.GetRaw(context.Background(), "/healthz", nil); err == nil || !strings.Contains(err.Error(), link) {
		t.Fatalf("link to a foreign socket from a world-writable directory: %v", err)
	}
}
