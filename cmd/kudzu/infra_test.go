package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
)

// unreadable fails to read one object, like a disk with a bad sector.
type unreadable struct {
	store.Objects
	id object.ID
}

func (u *unreadable) Get(ctx context.Context, id object.ID) ([]byte, error) {
	if id == u.id {
		return nil, errors.New("read error")
	}
	return u.Objects.Get(ctx, id)
}

// TestServeClosesIdleConnections: a keep-alive connection that sits idle is
// closed by the server instead of being held open forever.
func TestServeClosesIdleConnections(t *testing.T) {
	defer func(d time.Duration) { idleTimeout = d }(idleTimeout)
	idleTimeout = 100 * time.Millisecond
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveListener(ctx, l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	}()
	defer func() { cancel(); <-done }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: kudzu\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("idle connection still open: %v", err)
	}
}

// TestSnapshotInterrupted: when the server fails partway through a
// snapshot, kudzu snapshot fails and leaves no partial file to seed from.
func TestSnapshotInterrupted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	meta, err := store.OpenSQLite(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	objs := &unreadable{Objects: store.NewMem(4 << 20)}
	r := repo.New(meta, objs)
	srv := httptest.NewServer(server.New(r, log.New(io.Discard, "", 0)).Handler())
	defer srv.Close()

	big := make([]byte, 256<<10) // more than the server buffers
	rand.Read(big)
	var changes []object.Change
	for i, content := range [][]byte{big, []byte("b\n")} {
		id, err := r.WriteBlob(ctx, content)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, object.Change{Path: string(rune('a' + i)), Mode: object.ModeFile, Blob: id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Author: "a <a@example.com>", Message: "m", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	objs.id = changes[1].Blob

	out := filepath.Join(dir, "seed.frames")
	if err := cmdSnapshot(ctx, []string{"--remote", srv.URL, "-o", out}); err == nil {
		t.Fatal("snapshot succeeded although the server could not read an object")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial snapshot left behind: %v", err)
	}
}

// TestGuardBrowsers: a tier refuses what only a web page would send, cross-
// origin writes and (on loopback) Host names a DNS-rebinding page uses,
// while kudzu's own clients, which send neither, get through.
func TestGuardBrowsers(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveListener(ctx, l, ok) }()
	defer func() { cancel(); <-done }()
	addr := l.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	for _, tc := range []struct {
		name, method, host string
		header             map[string]string
		want               int
	}{
		{"client", "POST", addr, nil, http.StatusOK},
		{"localhost", "GET", "localhost:" + port, nil, http.StatusOK},
		{"ipv6", "GET", "[::1]:" + port, nil, http.StatusOK},
		{"same origin", "POST", addr, map[string]string{"Origin": "http://" + addr}, http.StatusOK},
		{"rebound read", "GET", "rebind.example:" + port, nil, http.StatusMisdirectedRequest},
		{"rebound write", "POST", "rebind.example:" + port, map[string]string{"Origin": "http://rebind.example:" + port}, http.StatusMisdirectedRequest},
		{"cross-origin write", "POST", addr, map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}, http.StatusForbidden},
		{"cross-site write", "POST", addr, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	} {
		req, _ := http.NewRequest(tc.method, "http://"+addr+"/api/refs/x", nil)
		req.Host = tc.host
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7070": true, "127.0.0.1": true, "[::1]:7070": true, "[::1]": true,
		"localhost:7070": true, "LOCALHOST.": true, "a.localhost:1": true, "10.1.2.3:80": true, "": true,
		"example.com:7070": false, "localhost.example.com": false, "127.0.0.1.nip.io:7070": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
