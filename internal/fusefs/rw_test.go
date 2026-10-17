//go:build fuse

package fusefs

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
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

// mountRW serves files on main and mounts it writable, with its overlay in
// overlayDir and its control socket running. It returns the mountpoint, the
// repository and a function that unmounts.
func mountRW(t *testing.T, r *repo.Repo, rem *proto.Remote, overlayDir string) (string, *vfs.Overlay, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	o, err := vfs.OpenOverlay(ctx, overlayDir, rem, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go o.ServeControl(ctx, nil)
	dir := t.TempDir()
	srv, err := MountWritable(dir, o, vfs.MountInfo{Control: vfs.ControlSocket(overlayDir), Remote: rem.Addr}, log.New(io.Discard, "", 0))
	if err != nil {
		cancel()
		t.Skipf("FUSE is not available here: %v", err)
	}
	done := false
	stop := func() {
		if !done {
			done = true
			srv.Unmount()
			cancel()
		}
	}
	t.Cleanup(stop)
	return dir, o, stop
}

func seedRepo(t *testing.T) (*repo.Repo, *proto.Remote) {
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
	return r, rem
}

func TestWritableMount(t *testing.T) {
	ctx := context.Background()
	r, rem := seedRepo(t)
	overlayDir := t.TempDir()
	dir, o, stop := mountRW(t, r, rem, overlayDir)

	// Everyday editing through the shell.
	sh(t, dir, "sh", "-c", `
		set -e
		echo "// appended" >> services/api/main.go
		sed -i 's/TODO: auth/DONE: auth/' services/api/handler.go
		printf 'package main\n' > services/api/new.go
		rm docs/guide.md
		mkdir -p notes/2026
		echo hello > notes/2026/today.md
		mv tools/run.sh tools/build.sh
		chmod -x tools/build.sh
		ln -s ../README.md notes/readme
		cp README.md README.copy
		truncate -s 0 README.copy
		touch README.md
	`)
	if got := sh(t, dir, "cat", "services/api/handler.go"); !strings.Contains(got, "DONE: auth") {
		t.Fatalf("sed -i result: %q", got)
	}
	if got := strings.Fields(sh(t, dir, "ls")); strings.Join(got, " ") != "README.copy README.md docs link notes services tools" {
		t.Fatalf("ls = %v", got)
	}
	if info, err := os.Stat(filepath.Join(dir, "tools/build.sh")); err != nil || info.Mode().Perm()&0o111 != 0 {
		t.Fatalf("chmod -x: %v %v", info.Mode(), err)
	}
	// Only the files that were edited, copied or renamed were fetched.
	if st := o.Base().Stats(); st.BlobsFetched != 4 { // main.go, handler.go, run.sh, README.md (cp)
		t.Fatalf("editing fetched %d blobs", st.BlobsFetched)
	}

	// kudzu status, diff and commit from inside the mount go through the
	// control socket the mount advertises.
	m, ok := vfs.FindMount(filepath.Join(dir, "services", "api"))
	if !ok || m.Info.Root != dir {
		t.Fatalf("FindMount = %+v, %v", m, ok)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range st.Files {
		got = append(got, f.Status+" "+f.Path)
	}
	want := "A README.copy|D docs/guide.md|A notes/2026/today.md|A notes/readme|M services/api/handler.go|M services/api/main.go|A services/api/new.go|A tools/build.sh|D tools/run.sh"
	if strings.Join(got, "|") != want {
		t.Fatalf("status:\n got  %s\n want %s", strings.Join(got, "|"), want)
	}
	if _, patch, err := m.Diff(ctx); err != nil || !strings.Contains(patch, "+// appended") {
		t.Fatalf("diff: %v\n%s", err, patch)
	}
	res, err := m.Commit(ctx, "edit in a writable mount", "agent <a@example.com>", "main")
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := r.Meta.GetRef(ctx, "main")
	if res.Land == nil || res.Base != tip {
		t.Fatalf("commit = %+v, main = %s", res, tip.Short())
	}
	if _, data, err := r.ReadFile(ctx, tip, "notes/2026/today.md"); err != nil || string(data) != "hello\n" {
		t.Fatalf("committed today.md = %q %v", data, err)
	}
	if e, _, err := r.ReadFile(ctx, tip, "tools/build.sh"); err != nil || e.Mode != object.ModeFile {
		t.Fatalf("committed build.sh = %+v %v", e, err)
	}
	if st, _ := m.Status(ctx); len(st.Files) != 0 || st.Changeset != tip {
		t.Fatalf("status after commit = %+v", st)
	}
	if got := sh(t, dir, "cat", "notes/2026/today.md"); got != "hello\n" {
		t.Fatalf("file after commit = %q", got)
	}

	// Uncommitted work survives an unmount: a new mount of the same overlay
	// resumes it.
	sh(t, dir, "sh", "-c", "echo draft > draft.txt")
	stop()
	dir2, _, _ := mountRW(t, r, rem, overlayDir)
	if got := sh(t, dir2, "cat", "draft.txt"); got != "draft\n" {
		t.Fatalf("draft after remount = %q", got)
	}
	m2, _ := vfs.FindMount(dir2)
	if st, _ := m2.Status(ctx); len(st.Files) != 1 || st.Files[0].Path != "draft.txt" {
		t.Fatalf("status after remount = %+v", st)
	}
	// Writing the reserved .kudzu directory is refused.
	if err := os.WriteFile(filepath.Join(dir2, ".kudzu", "x"), []byte("x"), 0o644); err == nil {
		t.Fatal("wrote into .kudzu")
	}
}
