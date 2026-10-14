// Package e2e runs a full stack (server -> regional cache -> node daemon over
// a unix socket -> working copies) in-process.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/garyships/kudzu/internal/auth"
	"github.com/garyships/kudzu/internal/client"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
	"github.com/garyships/kudzu/internal/tier"
)

type stack struct {
	repo   *repo.Repo
	server *httptest.Server
	cache  *tier.Tier
	daemon *tier.Tier
	sock   string
}

func newStack(t *testing.T) *stack { return newStackAuth(t, nil) }

// newStackAuth starts a stack whose server enforces policy (nil: no auth).
func newStackAuth(t *testing.T, policy *auth.Policy) *stack {
	t.Helper()
	dir := t.TempDir()
	r := repo.New(storetest.Meta(t), &store.Cached{Mem: store.NewMem(64 << 20), Backing: storetest.Objects(t)})
	s := &stack{repo: r}
	srv := server.New(r, log.New(io.Discard, "", 0))
	srv.Auth = policy
	s.server = httptest.NewServer(srv.Handler())
	t.Cleanup(s.server.Close)

	up, _ := proto.Dial(s.server.URL)
	s.cache = tier.New("cache", store.NewMem(64<<20), up, 4)
	cacheSrv := httptest.NewServer(s.cache.Handler())
	t.Cleanup(cacheSrv.Close)

	daemonObjs, err := store.OpenFS(filepath.Join(dir, "daemon"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	cacheUp, _ := proto.Dial(cacheSrv.URL)
	s.daemon = tier.New("daemon", daemonObjs, cacheUp, 4)
	// Unix socket paths are length-limited; keep it short.
	sockDir, err := os.MkdirTemp("", "gft")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	s.sock = filepath.Join(sockDir, "d.sock")
	l, err := net.Listen("unix", s.sock)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.daemon.Handler()}
	go hs.Serve(l)
	t.Cleanup(func() { hs.Close() })
	return s
}

func (s *stack) remote() string { return "unix://" + s.sock }

// seed creates an initial monorepo on main through the server API.
func (s *stack) seed(t *testing.T) object.ID {
	t.Helper()
	ctx := context.Background()
	files := map[string]string{
		"README.md":               "# monorepo\n",
		"services/api/main.go":    "package main\n\nfunc main() {\n\tserve()\n}\n",
		"services/api/handler.go": "package main\n\nfunc serve() {}\n",
		"services/web/index.html": "<h1>hi</h1>\n",
		"services/web/app.js":     "console.log('hi')\n",
		"docs/guide.md":           "guide\n",
		"tools/run.sh":            "#!/bin/sh\necho run\n",
	}
	var changes []object.Change
	for p, c := range files {
		id, err := s.repo.WriteBlob(ctx, []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		mode := object.ModeFile
		if strings.HasSuffix(p, ".sh") {
			mode = object.ModeExec
		}
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	c, err := s.repo.Create(ctx, repo.CreateRequest{Author: "seed", Message: "initial", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func listFiles(t *testing.T, root string) []string {
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if info.IsDir() {
			if rel == ".kudzu" {
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

func write(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, p)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFullStack(t *testing.T) {
	s := newStack(t)
	s.seed(t)
	ctx := context.Background()
	tmp := t.TempDir()

	// A sparse clone only fetches what it needs, and gets root files plus
	// everything under the chosen directory.
	sp, _ := sparse.Normalize([]string{"services/api"})
	a, err := client.Clone(ctx, s.remote(), filepath.Join(tmp, "a"), "main", sp, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(listFiles(t, a.Root), " ")
	if got != "README.md services/api/handler.go services/api/main.go" {
		t.Fatalf("sparse checkout = %s", got)
	}
	if info, err := os.Stat(filepath.Join(tmp, "a", "services/api/main.go")); err != nil || info.Size() == 0 {
		t.Fatal("file not materialized")
	}

	// A full clone through the same daemon reuses what the first one fetched.
	b, err := client.Clone(ctx, s.remote(), filepath.Join(tmp, "b"), "main", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(listFiles(t, b.Root)); n != 7 {
		t.Fatalf("full checkout has %d files", n)
	}
	if info, _ := os.Stat(filepath.Join(b.Root, "tools/run.sh")); info.Mode().Perm()&0o100 == 0 {
		t.Fatal("exec bit lost")
	}
	if st := s.daemon.Stats(); st.Hits == 0 {
		t.Fatalf("second clone should hit the daemon cache: %+v", st)
	}

	// Commit + land from A (fast-forward).
	write(t, a.Root, "services/api/handler.go", "package main\n\nfunc serve() { println(\"a\") }\n")
	write(t, a.Root, "services/api/new.go", "package main\n")
	st, _ := a.Status(ctx)
	if len(st) != 2 || st[0].Code != 'M' || st[1].Code != '?' {
		t.Fatalf("status = %+v", st)
	}
	res, err := a.Commit(ctx, "api: print a", "alice", true, "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.Land == nil || res.Land.Rebased {
		t.Fatalf("expected fast-forward land: %+v", res.Land)
	}

	// B commits on top of the old main; landing rebases it on the server.
	write(t, b.Root, "services/web/app.js", "console.log('b')\n")
	resB, err := b.Commit(ctx, "web: log b", "bob", false, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !resB.Land.Rebased {
		t.Fatal("expected B's land to be rebased")
	}
	// B's checkout moved to the landed changeset, so it now has A's change too.
	if data, _ := os.ReadFile(filepath.Join(b.Root, "services/api/new.go")); string(data) != "package main\n" {
		t.Fatal("B did not receive A's change after landing")
	}

	// A updates to main and gets B's change only if it is in its sparse cone.
	if err := a.Update(ctx, "main", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Root, "services/web/app.js")); err == nil {
		t.Fatal("update leaked files outside the sparse cone")
	}
	if err := a.SetSparse(ctx, sparse.Spec{"services/api", "services/web"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(a.Root, "services/web/app.js")); string(data) != "console.log('b')\n" {
		t.Fatalf("sparse add did not materialize B's change: %q", data)
	}

	// Conflicting edits: the second land is rejected with the path.
	write(t, a.Root, "README.md", "# monorepo (a)\n")
	if _, err := a.Commit(ctx, "readme a", "alice", false, ""); err != nil {
		t.Fatal(err)
	}
	write(t, b.Root, "README.md", "# monorepo (b)\n")
	if _, err := b.Commit(ctx, "readme b", "bob", false, "main"); err != nil {
		t.Fatal(err)
	}
	_, err = a.Land(ctx, "main")
	var he *proto.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict || len(he.Conflicts) != 1 || he.Conflicts[0] != "README.md" {
		t.Fatalf("expected README.md conflict, got %v", err)
	}

	// History queries need no working copy at all.
	rem, _ := proto.Dial(s.remote())
	var log []struct {
		Message string `json:"message"`
	}
	if err := rem.GetJSON(ctx, "/api/log", url.Values{"rev": {"main"}, "path": {"services/api"}}, &log); err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].Message != "api: print a" || log[1].Message != "initial" {
		t.Fatalf("path log = %+v", log)
	}
	var blame []struct {
		Line   int    `json:"line"`
		Author string `json:"author"`
	}
	if err := rem.GetJSON(ctx, "/api/blame", url.Values{"rev": {"main"}, "path": {"services/api/handler.go"}}, &blame); err != nil {
		t.Fatal(err)
	}
	if len(blame) != 3 || blame[0].Author != "seed" || blame[2].Author != "alice" {
		t.Fatalf("blame = %+v", blame)
	}
	var diff struct {
		Files []repo.FileDiff `json:"files"`
		Patch string          `json:"patch"`
	}
	if err := rem.GetJSON(ctx, "/api/diff", url.Values{"from": {"main~2"}, "to": {"main"}, "patch": {"1"}}, &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.Files) != 2 || !strings.Contains(diff.Patch, "+console.log('b')") {
		t.Fatalf("diff = %+v", diff)
	}
}

// TestCacheCoalescing: many cold requests for the same objects through one
// cache must reach the upstream only a handful of times.
func TestCacheCoalescing(t *testing.T) {
	s := newStack(t)
	s.seed(t)
	ctx := context.Background()
	var ids []object.ID
	tree, _ := s.repo.Resolve(ctx, "main")
	root, _ := s.repo.TreeOf(ctx, tree)
	s.repo.WalkObjects(ctx, root, nil, func(id object.ID) error {
		ids = append(ids, id)
		return nil
	})
	rem, _ := proto.Dial(s.remote())
	wave := func() {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n := 0
				missing, err := rem.Fetch(ctx, ids, func(id object.ID, b []byte) error {
					if _, _, err := object.Verify(id, b); err != nil {
						return err
					}
					n++
					return nil
				})
				if err != nil || len(missing) > 0 || n != len(ids) {
					t.Errorf("fetch: n=%d missing=%d err=%v", n, len(missing), err)
				}
			}()
		}
		wg.Wait()
	}
	wave() // cold: 50 simultaneous requests for the same objects
	st := s.daemon.Stats()
	if st.UpstreamObjects != int64(len(ids)) {
		t.Fatalf("daemon fetched %d objects upstream for %d distinct objects", st.UpstreamObjects, len(ids))
	}
	if cs := s.cache.Stats(); cs.UpstreamObjects != int64(len(ids)) {
		t.Fatalf("cache fetched %d objects upstream for %d distinct objects", cs.UpstreamObjects, len(ids))
	}
	t.Logf("cold wave: %d hits, %d misses (%d coalesced), %d upstream requests", st.Hits, st.Misses, st.Coalesced, st.UpstreamRequests)
	wave() // warm
	st2 := s.daemon.Stats()
	if st2.Hits-st.Hits != int64(50*len(ids)) || st2.UpstreamObjects != st.UpstreamObjects {
		t.Fatalf("warm wave should be all hits: %+v", st2)
	}
}

// TestConcurrentAgents: many agents each make a change in their own directory
// and land it at the same time, through the full stack.
func TestConcurrentAgents(t *testing.T) {
	s := newStack(t)
	s.seed(t)
	ctx := context.Background()
	tmp := t.TempDir()
	const agents = 12
	var wg sync.WaitGroup
	errs := make(chan error, agents)
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sp, _ := sparse.Normalize([]string{"services/api"})
			wc, err := client.Clone(ctx, s.remote(), filepath.Join(tmp, fmt.Sprint(i)), "main", sp, io.Discard)
			if err != nil {
				errs <- err
				return
			}
			write(t, wc.Root, fmt.Sprintf("services/api/agent%d.go", i), fmt.Sprintf("package main // %d\n", i))
			if _, err := wc.Commit(ctx, fmt.Sprintf("agent %d", i), "agent", true, "main"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	tip, _ := s.repo.Meta.GetRef(ctx, "main")
	log, _ := s.repo.Log(ctx, tip, 100)
	if len(log) != agents+1 {
		t.Fatalf("main has %d changesets, want %d", len(log), agents+1)
	}
}

// TestSnapshotSeed: a daemon seeded from a snapshot serves a checkout of that
// revision without asking upstream for anything.
func TestSnapshotSeed(t *testing.T) {
	s := newStack(t)
	s.seed(t)
	ctx := context.Background()
	rem, _ := proto.Dial(s.server.URL)
	body, err := rem.Stream(ctx, "/api/snapshot", url.Values{"rev": {"main"}})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	objs, err := store.OpenFS(filepath.Join(t.TempDir(), "seeded"), 0)
	if err != nil {
		t.Fatal(err)
	}
	seeded := tier.New("daemon", objs, rem, 4)
	n, err := seeded.Seed(body)
	if err != nil || n == 0 {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}
	srv := httptest.NewServer(seeded.Handler())
	defer srv.Close()
	if _, err := client.Clone(ctx, srv.URL, filepath.Join(t.TempDir(), "wc"), "main", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if st := seeded.Stats(); st.UpstreamObjects != 0 || st.Hits == 0 {
		t.Fatalf("seeded daemon went upstream: %+v", st)
	}
}

// TestUpdateForce: --force discards local edits, including edits to files
// that are the same in both revisions.
func TestUpdateForce(t *testing.T) {
	s := newStack(t)
	s.seed(t)
	ctx := context.Background()
	wc, err := client.Clone(ctx, s.remote(), filepath.Join(t.TempDir(), "wc"), "main", nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	write(t, wc.Root, "README.md", "local edit\n")
	os.Remove(filepath.Join(wc.Root, "docs/guide.md"))
	if err := wc.Update(ctx, "main", false); err == nil {
		t.Fatal("update without --force should refuse a dirty working copy")
	}
	if err := wc.Update(ctx, "main", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wc.Root, "README.md")); string(b) != "# monorepo\n" {
		t.Fatalf("README.md not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(wc.Root, "docs/guide.md")); string(b) != "guide\n" {
		t.Fatalf("docs/guide.md not restored: %q", b)
	}
	if st, _ := wc.Status(ctx); len(st) != 0 {
		t.Fatalf("working copy still dirty: %+v", st)
	}
}
