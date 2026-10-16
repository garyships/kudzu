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
