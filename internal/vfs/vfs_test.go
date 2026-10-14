package vfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

var files = map[string]string{
	"README.md":            "# repo\n",
	"services/api/main.go": "package main\n\nfunc main() {}\n",
	"services/api/util.go": "package main\n\nfunc util() {}\n",
	"services/web/app.js":  strings.Repeat("console.log('web')\n", 100),
	"tools/run.sh":         "#!/bin/sh\necho run\n",
	"link":                 "services/api/main.go",
	"vendor/sub":           "0123456789abcdef0123456789abcdef01234567",
}

// serve starts a server holding files on main and returns a remote for it
// and a function reporting how many objects the server has sent.
func serve(t *testing.T) (*proto.Remote, func() int64) {
	t.Helper()
	ctx := context.Background()
	r := repo.New(storetest.Meta(t), &store.Cached{Mem: store.NewMem(16 << 20), Backing: storetest.Objects(t)})
	var changes []object.Change
	for p, content := range files {
		id, err := r.WriteBlob(ctx, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		mode := object.ModeFile
		switch p {
		case "tools/run.sh":
			mode = object.ModeExec
		case "link":
			mode = object.ModeSymlink
		case "vendor/sub":
			mode = object.ModeGitlink
		}
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Message: "init", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(r, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	rem, _ := proto.Dial(srv.URL)
	served := func() int64 {
		var st struct {
			ObjectsServed int64 `json:"objects_served"`
		}
		if err := rem.GetJSON(ctx, "/stats", nil, &st); err != nil {
			t.Fatal(err)
		}
		return st.ObjectsServed
	}
	return rem, served
}

// TestLazy: listing fetches only tree objects, reading a file fetches only
// its blob, and nothing is fetched twice.
func TestLazy(t *testing.T) {
	rem, served := serve(t)
	ctx := context.Background()
	v, err := Open(ctx, rem, "main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if served() != 0 {
		t.Fatal("opening a revision fetched objects")
	}
	root, err := v.ReadDir(ctx, v.Root())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range root {
		names = append(names, n.Path)
	}
	if got := strings.Join(names, " "); got != "README.md link services tools vendor" {
		t.Fatalf("root = %s", got)
	}
	if st := v.Stats(); st.TreesFetched != 1 || st.BlobsFetched != 0 {
		t.Fatalf("after listing the root: %+v", st)
	}

	n, err := v.Stat(ctx, "services/api/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if n.Size != int64(len(files["services/api/main.go"])) || n.Mode != object.ModeFile {
		t.Fatalf("stat = %+v", n)
	}
	if st := v.Stats(); st.TreesFetched != 3 || st.BlobsFetched != 0 {
		t.Fatalf("stat should fetch only the trees on the path: %+v", st)
	}
	for i := 0; i < 3; i++ {
		b, err := v.ReadFile(ctx, n)
		if err != nil || string(b) != files["services/api/main.go"] {
			t.Fatalf("read = %q, %v", b, err)
		}
	}
	if st := v.Stats(); st.TreesFetched != 3 || st.BlobsFetched != 1 {
		t.Fatalf("after reading one file three times: %+v", st)
	}
	if got := served(); got != 4 {
		t.Fatalf("server sent %d objects for 3 trees and 1 blob", got)
	}

	if l, _ := v.Stat(ctx, "link"); l.Mode != object.ModeSymlink || l.Size != int64(len(files["link"])) {
		t.Fatalf("symlink = %+v", l)
	}
	if x, _ := v.Stat(ctx, "tools/run.sh"); x.Mode != object.ModeExec {
		t.Fatalf("exec bit lost: %+v", x)
	}
	if _, err := v.Stat(ctx, "vendor/sub"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("submodules are not files: %v", err)
	}
	if _, err := v.Stat(ctx, "services/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path: %v", err)
	}
	if _, err := v.ReadDir(ctx, n); err == nil {
		t.Fatal("listing a file should fail")
	}
}

func TestSparse(t *testing.T) {
	rem, _ := serve(t)
	ctx := context.Background()
	sp, _ := sparse.Normalize([]string{"services/api"})
	v, err := Open(ctx, rem, "main", sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Stat(ctx, "services/web/app.js"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outside the cone: %v", err)
	}
	if _, err := v.Stat(ctx, "tools"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outside the cone: %v", err)
	}
	if _, err := v.Stat(ctx, "README.md"); err != nil {
		t.Fatalf("root files stay visible: %v", err)
	}
	if _, err := v.Stat(ctx, "services/api/util.go"); err != nil {
		t.Fatal(err)
	}
}
