//go:build unix

package proto

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// serveSocket serves on a unix socket at path, answering "ok" and
// recording the last Authorization header it saw.
func serveSocket(t *testing.T, path string) *string {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var auth string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return &auth
}

// fakeOwners makes socketOwner report owners by base name; other files
// keep their real owner.
func fakeOwners(t *testing.T, owners map[string]int) {
	saved := socketOwner
	socketOwner = func(fi os.FileInfo) (int, bool) {
		if uid, ok := owners[fi.Name()]; ok {
			return uid, true
		}
		return saved(fi)
	}
	t.Cleanup(func() { socketOwner = saved })
}

// shortTempDir is a temporary directory with a path short enough for a
// unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kzs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func get(t *testing.T, addr string) error {
	t.Helper()
	r, err := Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.GetRaw(context.Background(), "/healthz", nil)
	return err
}

// TestSocketOwnerCheck serves on a socket in a sticky, world-writable
// directory like /tmp and pretends different users own it.
func TestSocketOwnerCheck(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "d.sock")
	serveSocket(t, sock)
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	owners := map[string]int{"d.sock": os.Getuid(), "l.sock": os.Getuid()}
	fakeOwners(t, owners)
	setOwner := func(uid int) { owners["d.sock"], owners["l.sock"] = uid, uid }

	if err := get(t, "unix://"+sock); err != nil {
		t.Fatalf("socket of the current user: %v", err)
	}
	setOwner(0)
	if err := get(t, "unix://"+sock); err != nil {
		t.Fatalf("socket of root: %v", err)
	}
	other := os.Getuid() + 4242
	setOwner(other)
	err := get(t, "unix://"+sock)
	if err == nil {
		t.Fatal("a socket another user owns in a world-writable directory was accepted")
	}
	for _, want := range []string{sock, "uid " + strconv.Itoa(other), "world-writable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	// The same socket in a directory only its owner can write to is fine.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := get(t, "unix://"+sock); err != nil {
		t.Fatalf("socket in a private directory: %v", err)
	}

	// A link from a world-writable directory is checked as well.
	pub := shortTempDir(t)
	if err := os.Chmod(pub, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pub, "l.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if err := get(t, "unix://"+link); err == nil || !strings.Contains(err.Error(), link) {
		t.Fatalf("link to a foreign socket from a world-writable directory: %v", err)
	}
}

// TestSocketInForeignDirectory: another user's socket in a directory of
// theirs is only as safe as where that directory is. Inside a
// world-writable one (an attacker's /tmp/x/) anyone could have made it;
// inside one only root or the current user can write to (a daemon's
// directory under /run) they could not.
func TestSocketInForeignDirectory(t *testing.T) {
	other := os.Getuid() + 4242
	for _, c := range []struct {
		name   string
		mode   os.FileMode
		refuse bool
	}{{"world-writable", 0o777 | os.ModeSticky, true}, {"private", 0o755, false}} {
		t.Run(c.name, func(t *testing.T) {
			top := shortTempDir(t)
			theirs := filepath.Join(top, "x")
			os.Mkdir(theirs, 0o755)
			sock := filepath.Join(theirs, "d.sock")
			serveSocket(t, sock)
			if err := os.Chmod(top, c.mode); err != nil {
				t.Fatal(err)
			}
			fakeOwners(t, map[string]int{"x": other, "d.sock": other})
			err := get(t, "unix://"+sock)
			if c.refuse && (err == nil || !strings.Contains(err.Error(), "any local user can create files in "+top)) {
				t.Fatalf("another user's socket in their directory in a world-writable one: %v", err)
			}
			if !c.refuse && err != nil {
				t.Fatalf("another user's socket in their directory in a private one: %v", err)
			}
		})
	}
}

// TestAbstractSocketRefused: an abstract socket has no owner and no
// permissions, so any local user could have bound the name first.
func TestAbstractSocketRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract sockets are Linux only")
	}
	name := "@kudzu-test-" + strconv.Itoa(os.Getpid())
	serveSocket(t, name)
	if err := get(t, "unix://"+name); err == nil || !strings.Contains(err.Error(), "abstract") {
		t.Fatalf("abstract socket: %v", err)
	}
}

// TestDialPrivate: a private socket, like a writable mount's control
// socket, gets no token, and is refused unless it is the current user's
// and, where the system says, so is the process serving it.
func TestDialPrivate(t *testing.T) {
	t.Setenv("KUDZU_TOKEN", "secret")
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "c.sock")
	auth := serveSocket(t, sock)
	owners := map[string]int{}
	fakeOwners(t, owners)
	call := func() error {
		t.Helper()
		r, err := DialPrivate(sock)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.GetRaw(context.Background(), "/base", nil)
		return err
	}

	if err := call(); err != nil || *auth != "" {
		t.Fatalf("own socket: %v, Authorization %q", err, *auth)
	}
	owners["c.sock"] = os.Getuid() + 4242
	if err := call(); err == nil || !strings.Contains(err.Error(), "not a socket of yours") {
		t.Fatalf("another user's socket: %v", err)
	}
	delete(owners, "c.sock")

	if runtime.GOOS == "linux" {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		uid, err := peerUID(c)
		c.Close()
		if err != nil || uid != os.Getuid() {
			t.Fatalf("peerUID = %d, %v; want %d", uid, err, os.Getuid())
		}
	}
	saved := peerUID
	t.Cleanup(func() { peerUID = saved })
	peerUID = func(net.Conn) (int, error) { return os.Getuid() + 4242, nil }
	if err := call(); err == nil || !strings.Contains(err.Error(), "served by uid") {
		t.Fatalf("socket served by another user: %v", err)
	}
	peerUID = func(net.Conn) (int, error) { return 0, errors.New("no credentials") }
	if err := call(); err == nil || !strings.Contains(err.Error(), "cannot tell who serves it") {
		t.Fatalf("socket with unknown server: %v", err)
	}

	for _, bad := range []string{"c.sock", "@kudzu-control"} {
		if _, err := DialPrivate(bad); err == nil {
			t.Fatalf("DialPrivate(%q) accepted a path that is not absolute", bad)
		}
	}
}
