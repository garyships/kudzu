//go:build fuse

package fusefs

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
	"github.com/garyships/kudzu/internal/vfs"
)

var files = map[string]string{
	"README.md":               "# repo\n",
	"services/api/main.go":    "package main\n\nfunc main() { serve() }\n",
	"services/api/handler.go": "package main\n\nfunc serve() {} // TODO: auth\n",
	"services/web/app.js":     "// TODO: web\nconsole.log('web')\n",
	"docs/guide.md":           "guide\n",
	"tools/run.sh":            "#!/bin/sh\necho run\n",
	"link":                    "services/api/main.go",
}

// mount serves files from a server and mounts main at a temporary
// directory, skipping the test where FUSE is unavailable.
func mount(t *testing.T) (string, *vfs.FS) {
	t.Helper()
	ctx := context.Background()
	r := repo.New(storetest.Meta(t), &store.Cached{Mem: store.NewMem(16 << 20), Backing: storetest.Objects(t)})
	var changes []object.Change
	for p, content := range files {
		id, _ := r.WriteBlob(ctx, []byte(content))
		mode := object.ModeFile
		switch p {
		case "tools/run.sh":
			mode = object.ModeExec
		case "link":
			mode = object.ModeSymlink
		}
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Message: "init", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	r.Meta.CASRef(ctx, "main", object.Zero, c.ID)
	srv := httptest.NewServer(server.New(r, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	rem, _ := proto.Dial(srv.URL)
	v, err := vfs.Open(ctx, rem, "main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fsrv, err := Mount(dir, v, log.New(io.Discard, "", 0))
	if err != nil {
		t.Skipf("FUSE is not available here: %v", err)
	}
	t.Cleanup(func() { fsrv.Unmount() })
	return dir, v
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestMount(t *testing.T) {
	dir, v := mount(t)

	// ls lists from tree objects alone; ls -l gets exact sizes without
	// fetching a single file.
	if got := strings.Fields(sh(t, dir, "ls")); strings.Join(got, " ") != "README.md docs link services tools" {
		t.Fatalf("ls = %v", got)
	}
	long := sh(t, dir, "ls", "-l", "services/api")
	if !strings.Contains(long, "-r--r--r--") || !strings.Contains(long, " 44 ") || !strings.Contains(long, " 38 ") {
		t.Fatalf("ls -l = %s", long)
	}
	if info, err := os.Stat(filepath.Join(dir, "tools/run.sh")); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("exec bit: %v %v", info.Mode(), err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "README.md")); info.ModTime().Unix() != v.Time {
		t.Fatalf("mtime %v, want the changeset's time %d", info.ModTime(), v.Time)
	}
	if st := v.Stats(); st.BlobsFetched != 0 {
		t.Fatalf("listing fetched file contents: %+v", st)
	}

	// cat fetches exactly the file it reads.
	if got := sh(t, dir, "cat", "services/api/handler.go"); got != files["services/api/handler.go"] {
		t.Fatalf("cat = %q", got)
	}
	if st := v.Stats(); st.BlobsFetched != 1 {
		t.Fatalf("after cat: %+v", st)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != files["link"] {
		t.Fatalf("readlink = %q %v", target, err)
	}

	// grep -r over one directory reads only that directory's files.
	if got := sh(t, dir, "grep", "-rn", "TODO", "services/api"); strings.TrimSpace(got) != "services/api/handler.go:3:func serve() {} // TODO: auth" {
		t.Fatalf("grep -r = %q", got)
	}
	st := v.Stats()
	if st.BlobsFetched != 3 { // handler.go (cached), main.go, plus the symlink target read above
		t.Fatalf("after grep -r services/api: %+v", st)
	}
	if out := sh(t, dir, "grep", "-rl", "TODO", "."); strings.Fields(out)[0] != "./services/api/handler.go" || len(strings.Fields(out)) != 2 {
		t.Fatalf("grep -rl = %q", out)
	}
	if st := v.Stats(); st.BlobsFetched != int64(len(files)) {
		t.Fatalf("grep -r over everything should fetch each blob once: %+v", st)
	}

	// The mount is read-only.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err == nil {
		t.Fatal("wrote to a read-only mount")
	}
}
