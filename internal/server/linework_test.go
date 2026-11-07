package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

// lineWorkServer serves a repository whose main has the given files, and
// reports the path of each request its handler has finished with.
func lineWorkServer(t *testing.T, set map[string]string) (*Server, *repo.Repo, *proto.Remote, <-chan string) {
	t.Helper()
	ctx := t.Context()
	r := repo.New(storetest.Meta(t), store.NewMem(16<<20))
	srv := New(r, log.New(io.Discard, "", 0))
	done := make(chan string, 16)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		srv.Handler().ServeHTTP(w, req)
		done <- req.URL.Path
	}))
	t.Cleanup(hs.Close)
	var changes []object.Change
	for p, content := range set {
		id, err := r.WriteBlob(ctx, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, object.Change{Path: p, Mode: object.ModeFile, Blob: id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Author: "a <a@example.com>", Message: "m", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	rem, err := proto.Dial(hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, r, rem, done
}

// TestLineWorkRequestsWaitForSlot: blame and patch requests wait for one of
// a few slots, so that parallel requests can't multiply what each holds,
// and stop waiting when their client goes away.
func TestLineWorkRequestsWaitForSlot(t *testing.T) {
	srv, _, rem, done := lineWorkServer(t, map[string]string{"f": "a\nb\n"})
	srv.heavy = make(chan struct{}, 1)
	srv.heavy <- struct{}{} // the only slot is busy
	requests := []struct {
		path string
		q    url.Values
	}{
		{"/api/blame", url.Values{"path": {"f"}}},
		{"/api/diff", url.Values{"patch": {"1"}}},
	}
	for _, req := range requests {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err := rem.GetRaw(ctx, req.path, req.q)
		cancel()
		if err == nil {
			t.Fatalf("%s was answered while every slot was busy", req.path)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s still waits for a slot after its client went away", req.path)
		}
	}
	if _, err := rem.GetRaw(t.Context(), "/api/diff", nil); err != nil {
		t.Fatalf("a diff without a patch needs no slot: %v", err)
	}
	<-srv.heavy
	for _, req := range requests {
		if _, err := rem.GetRaw(t.Context(), req.path, req.q); err != nil {
			t.Fatalf("%s: %v", req.path, err)
		}
	}
	if n := len(srv.heavy); n != 0 {
		t.Fatalf("%d slots still taken", n)
	}
}

// TestLineWorkResponses: blame and patch responses, written a piece at a
// time, are the same JSON as when they were encoded whole.
func TestLineWorkResponses(t *testing.T) {
	text := "if a < b && c > d {\n\tx := \"é \"\n}\n\x01no newline"
	_, r, rem, _ := lineWorkServer(t, map[string]string{"f": text})
	ctx := t.Context()
	head, err := r.Resolve(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	encode := func(v any) []byte {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.Encode(v)
		return buf.Bytes()
	}

	got, err := rem.GetRaw(ctx, "/api/blame", url.Values{"path": {"f"}})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := r.Blame(ctx, head, "f")
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Changeset(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	var want []BlameLine
	for _, l := range lines {
		want = append(want, BlameLine{BlameLine: l, Author: c.Author, Time: c.Time})
	}
	if len(want) != 4 || !bytes.Equal(got, encode(want)) {
		t.Fatalf("blame response\n%s\nwant\n%s", got, encode(want))
	}

	got, err = rem.GetRaw(ctx, "/api/diff", url.Values{"to": {"main"}, "patch": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	files, err := r.Diff(ctx, object.Zero, head, "")
	if err != nil {
		t.Fatal(err)
	}
	patch, err := r.Patch(ctx, files)
	if err != nil {
		t.Fatal(err)
	}
	if wantDiff := encode(DiffResponse{To: head, Files: files, Patch: patch}); !bytes.Equal(got, wantDiff) {
		t.Fatalf("diff response\n%s\nwant\n%s", got, wantDiff)
	}
}

// TestWriteJSONString: a string written a piece at a time is escaped as it
// would be whole, also when a piece ends inside a multi-byte rune or in
// invalid UTF-8.
func TestWriteJSONString(t *testing.T) {
	for _, s := range []string{
		"",
		"plain",
		strings.Repeat("a", 32<<10-1) + "é" + strings.Repeat("\x01\"\\<\n", 5000),
		strings.Repeat("a", 32<<10-2) + "€€€" + strings.Repeat("😀", 20000),
		strings.Repeat("\xe2\x82", 40000) + "x",
		strings.Repeat("\xbf", 70000),
	} {
		var whole bytes.Buffer
		enc := json.NewEncoder(&whole)
		enc.SetEscapeHTML(false)
		enc.Encode(s)
		var pieces bytes.Buffer
		if err := writeJSONString(&pieces, s); err != nil {
			t.Fatal(err)
		}
		pieces.WriteByte('\n')
		if !bytes.Equal(pieces.Bytes(), whole.Bytes()) {
			t.Fatalf("string of %d bytes escaped differently in pieces", len(s))
		}
	}
}
