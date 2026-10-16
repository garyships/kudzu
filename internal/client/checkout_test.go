package client

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
)

// hostile stands in for a compromised server, cache or daemon: /api/resolve
// names root, and the object protocol serves whatever objects it was given,
// in whatever form. Objects named in last are sent after a pause, so the
// order in which a client writes files is predictable.
type hostile struct {
	objs map[object.ID][]byte
	root object.ID
	last map[object.ID]bool
}

func newHostile() *hostile {
	return &hostile{objs: map[object.ID][]byte{}, last: map[object.ID]bool{}}
}

func (h *hostile) blob(content string) object.ID {
	id, stored := object.EncodeObject(object.KindBlob, []byte(content))
	h.objs[id] = stored
	return id
}

// tree encodes entries as given: unsorted, repeated or reserved names too.
func (h *hostile) tree(es ...object.Entry) object.ID {
	id, stored := object.EncodeObject(object.KindTree, (&object.Tree{Entries: es}).Encode())
	h.objs[id] = stored
	return id
}

func (h *hostile) serve(t *testing.T) string {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/resolve", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]object.ID{"changeset": object.Hash(object.KindChangeset, []byte("x")), "tree": h.root})
	})
	mux.HandleFunc("POST /objects/fetch", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ids, _ := proto.DecodeIDs(b)
		var late []object.ID
		for _, id := range ids {
			if h.last[id] {
				late = append(late, id)
			} else if stored, ok := h.objs[id]; ok {
				proto.WriteFrame(w, id, stored)
			} else {
				proto.WriteMissing(w, id)
			}
		}
		if len(late) > 0 {
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
			for _, id := range late {
				proto.WriteFrame(w, id, h.objs[id])
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func file(name string, id object.ID) object.Entry {
	return object.Entry{Name: name, Mode: object.ModeFile, ID: id}
}

func dir(name string, id object.ID) object.Entry {
	return object.Entry{Name: name, Mode: object.ModeDir, ID: id}
}

// newServer starts a real server; wrap, if set, sees every request first.
func newServer(t *testing.T, wrap func(http.ResponseWriter, *http.Request)) (*repo.Repo, string) {
	meta, err := store.OpenSQLite(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meta.Close() })
	r := repo.New(meta, store.NewMem(64<<20))
	h := server.New(r, log.New(io.Discard, "", 0)).Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if wrap != nil {
			wrap(w, req)
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return r, srv.URL
}

// commit creates a changeset on parent (zero for a root) and moves main to
// it. An empty content deletes the path.
func commit(t *testing.T, r *repo.Repo, parent object.ID, files map[string]string) object.ID {
	t.Helper()
	ctx := context.Background()
	var changes []object.Change
	for p, c := range files {
		ch := object.Change{Path: p}
		if c != "" {
			id, err := r.WriteBlob(ctx, []byte(c))
			if err != nil {
				t.Fatal(err)
			}
			ch.Mode, ch.Blob = object.ModeFile, id
		}
		changes = append(changes, ch)
	}
	var parents []object.ID
	if !parent.IsZero() {
		parents = []object.ID{parent}
	}
	c, err := r.Create(ctx, repo.CreateRequest{Parents: parents, Author: "a", Message: "m", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", parent, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func clone(t *testing.T, remote string, sp sparse.Spec) *WC {
	t.Helper()
	wc, err := Clone(context.Background(), remote, filepath.Join(t.TempDir(), "wc"), "main", sp, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return wc
}

func writeTo(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A nested .git/config can make any git command run in that directory
// execute a program (core.fsmonitor); .kudzu holds the working copy's state.
func TestCloneRefusesReservedNames(t *testing.T) {
	for _, name := range []string{".git", ".GIT", ".git.", ".git ", "git~1", ".kudzu", ".Kudzu"} {
		h := newHostile()
		config := h.blob("[core]\n\tfsmonitor = \"touch /tmp/pwned; false\"\n")
		sub := h.tree(dir(name, h.tree(file("HEAD", h.blob("ref: refs/heads/main\n")), file("config", config))))
		h.root = h.tree(file("README", h.blob("hi\n")), dir("sub", sub))
		dst := filepath.Join(t.TempDir(), "wc")
		_, err := Clone(context.Background(), h.serve(t), dst, "main", nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "sub/"+name) {
			t.Errorf("clone with sub/%s: err = %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dst, "sub", name, "config")); err == nil {
			t.Errorf("clone wrote sub/%s/config", name)
		}
	}
}

// A git checkout nested in the working copy is not part of it: no changeset
// may hold its .git, so status and commit -A leave it out.
func TestStatusIgnoresNestedGit(t *testing.T) {
	r, remote := newServer(t, nil)
	commit(t, r, object.Zero, map[string]string{"README": "hi\n"})
	wc := clone(t, remote, nil)
	writeTo(t, wc.Root, "vendor/lib/.git/HEAD", "ref: refs/heads/main\n")
	writeTo(t, wc.Root, "vendor/lib/lib.go", "package lib\n")
	writeTo(t, wc.Root, "vendor/sub/.git", "gitdir: ../../.git/modules/sub\n")
	ctx := context.Background()
	st, err := wc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Path != "vendor/lib/lib.go" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := wc.Commit(ctx, "vendor lib", "a", true, ""); err != nil {
		t.Fatal(err)
	}
}
