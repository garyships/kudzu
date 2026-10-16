package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
)

// serveRepo is serve, also returning the repository.
func serveRepo(t *testing.T) (*repo.Repo, *proto.Remote) {
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
	return r, rem
}

func write(t *testing.T, o *Overlay, p, content string, create, trunc bool) {
	t.Helper()
	f, err := o.OpenWrite(context.Background(), p, create, false, trunc, false)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := o.CloseWrite(f); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, o *Overlay, dir string) string {
	t.Helper()
	ents, err := o.ReadDir(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Path)
	}
	return strings.Join(out, " ")
}

func TestOverlayEditsAndCommit(t *testing.T) {
	ctx := context.Background()
	r, rem := serveRepo(t)
	dir := t.TempDir()
	o, err := OpenOverlay(ctx, dir, rem, "main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := o.Base()

	// Append to a base file (copies it up), create files, delete, rename,
	// make directories and links.
	write(t, o, "services/api/main.go", "// edited\n", false, false)
	write(t, o, "services/api/new.go", "package main\n", true, false)
	if err := o.Remove(ctx, "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := o.Mkdir(ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := o.Symlink(ctx, "../services/api/main.go", "notes/link"); err != nil {
		t.Fatal(err)
	}
	if err := o.Rename(ctx, "tools/run.sh", "tools/run2.sh"); err != nil {
		t.Fatal(err)
	}
	if err := o.Rmdir(ctx, "services/web"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("rmdir of a non-empty directory = %v", err)
	}
	if err := o.Remove(ctx, "services/web/app.js"); err != nil {
		t.Fatal(err)
	}
	if err := o.Rmdir(ctx, "services/web"); err != nil {
		t.Fatal(err)
	}
	if err := o.Rename(ctx, "services", "svc"); !errors.Is(err, ErrCrossDir) {
		t.Fatalf("renaming a base directory = %v", err)
	}
	if err := o.Mkdir(ctx, "notes"); !errors.Is(err, ErrExist) {
		t.Fatalf("mkdir of an existing directory = %v", err)
	}
	if _, err := o.OpenWrite(ctx, ".kudzu/x", true, false, false, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("creating a reserved path = %v", err)
	}

	if got := names(t, o, ""); got != "link notes services tools vendor" {
		t.Fatalf("root = %s", got)
	}
	if got := names(t, o, "services"); got != "services/api" {
		t.Fatalf("services = %s", got)
	}
	if got := names(t, o, "services/api"); got != "services/api/main.go services/api/new.go services/api/util.go" {
		t.Fatalf("services/api = %s", got)
	}
	if e, err := o.Stat(ctx, "tools/run2.sh"); err != nil || e.Mode != object.ModeExec {
		t.Fatalf("renamed script = %+v, %v", e, err)
	}
	if _, err := o.Stat(ctx, "README.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted file = %v", err)
	}
	b, err := o.ReadFile(ctx, "services/api/main.go")
	if err != nil || string(b) != files["services/api/main.go"]+"// edited\n" {
		t.Fatalf("edited file = %q, %v", b, err)
	}
	// Only the two files that were copied up were fetched.
	if st := base.Stats(); st.BlobsFetched != 2 {
		t.Fatalf("editing fetched %d blobs", st.BlobsFetched)
	}

	changes, err := o.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		got = append(got, c.Status+" "+c.Path)
	}
	want := "D README.md|A notes/link|M services/api/main.go|A services/api/new.go|D services/web/app.js|D tools/run.sh|A tools/run2.sh"
	if strings.Join(got, "|") != want {
		t.Fatalf("changes:\n got  %s\n want %s", strings.Join(got, "|"), want)
	}
	if _, patch, err := o.Diff(ctx); err != nil || !strings.Contains(patch, "+// edited") || !strings.Contains(patch, "-# repo") {
		t.Fatalf("diff = %q, %v", patch, err)
	}

	res, err := o.Commit(ctx, "edit through the overlay", "agent <a@example.com>", "main")
	if err != nil {
		t.Fatal(err)
	}
	if res.Changes != 7 || res.Land == nil || res.Base != res.Land.Landed {
		t.Fatalf("commit = %+v", res)
	}
	tip, _ := r.Meta.GetRef(ctx, "main")
	if tip != res.Base || o.Base().Changeset != tip {
		t.Fatal("main or the overlay's base did not move")
	}
	if _, data, err := r.ReadFile(ctx, tip, "services/api/main.go"); err != nil || !strings.HasSuffix(string(data), "// edited\n") {
		t.Fatalf("committed main.go = %q, %v", data, err)
	}
	if e, _, err := r.ReadFile(ctx, tip, "tools/run2.sh"); err != nil || e.Mode != object.ModeExec {
		t.Fatalf("committed run2.sh = %+v, %v", e, err)
	}
	if _, _, err := r.ReadFile(ctx, tip, "services/web/app.js"); err == nil {
		t.Fatal("deleted file still on main")
	}
	if changes, _ := o.Changes(ctx); len(changes) != 0 {
		t.Fatalf("overlay not empty after commit: %+v", changes)
	}
	if e, err := o.Stat(ctx, "services/api/new.go"); err != nil || e.Upper {
		t.Fatalf("committed file should come from the base now: %+v, %v", e, err)
	}
	if _, err := o.Commit(ctx, "again", "agent", ""); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("empty commit = %v", err)
	}
}

// TestOverlayReplaceDirectoryWithFile: deleting a directory and creating a
// file in its place, and the reverse, become the right changes.
func TestOverlayTypeChanges(t *testing.T) {
	ctx := context.Background()
	r, rem := serveRepo(t)
	o, err := OpenOverlay(ctx, t.TempDir(), rem, "main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"tools/run.sh"} {
		if err := o.Remove(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Rmdir(ctx, "tools"); err != nil {
		t.Fatal(err)
	}
	write(t, o, "tools", "now a file\n", true, false)
	if err := o.Remove(ctx, "README.md"); err != nil {
		t.Fatal(err)
	}
	if err := o.Mkdir(ctx, "README.md"); err != nil {
		t.Fatal(err)
	}
	write(t, o, "README.md/index.md", "hi\n", true, false)
	if _, err := o.Stat(ctx, "tools/run.sh"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("file below a replaced directory = %v", err)
	}
	res, err := o.Commit(ctx, "swap", "agent", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, data, err := r.ReadFile(ctx, res.Base, "tools"); err != nil || string(data) != "now a file\n" {
		t.Fatalf("tools = %q, %v", data, err)
	}
	if _, data, err := r.ReadFile(ctx, res.Base, "README.md/index.md"); err != nil || string(data) != "hi\n" {
		t.Fatalf("README.md/index.md = %q, %v", data, err)
	}
}

func TestOverlayResumeAndUpdate(t *testing.T) {
	ctx := context.Background()
	r, rem := serveRepo(t)
	dir := t.TempDir()
	o, err := OpenOverlay(ctx, dir, rem, "main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, o, "services/api/util.go", "// more\n", false, false)
	if err := o.Remove(ctx, "README.md"); err != nil {
		t.Fatal(err)
	}
	start := o.Base().Changeset

	// Someone else lands a change.
	blob, _ := r.WriteBlob(ctx, []byte("other\n"))
	c, err := r.Create(ctx, repo.CreateRequest{Parents: []object.ID{start}, Message: "other", Changes: []object.Change{{Path: "other.txt", Mode: object.ModeFile, Blob: blob}}})
	if err != nil {
		t.Fatal(err)
	}
	r.Meta.CASRef(ctx, "main", start, c.ID)

	// A new process resumes the overlay where it was.
	o2, err := OpenOverlay(ctx, dir, rem, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o2.Base().Changeset != start {
		t.Fatal("resumed on a different base")
	}
	if changes, _ := o2.Changes(ctx); len(changes) != 2 {
		t.Fatalf("resumed changes = %+v", changes)
	}
	if _, err := OpenOverlay(ctx, dir, rem, "main", nil, nil); err == nil {
		t.Fatal("moving a dirty overlay to another revision should fail")
	}
	f, err := o2.OpenWrite(ctx, "services/api/util.go", false, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o2.Commit(ctx, "busy", "agent", ""); err == nil || !strings.Contains(err.Error(), "open for writing") {
		t.Fatalf("commit with a file open for writing = %v", err)
	}
	if err := o2.Update(ctx, "main", false); err == nil {
		t.Fatal("update of a dirty overlay should fail")
	}
	o2.CloseWrite(f)
	if err := o2.Update(ctx, "main", true); err != nil {
		t.Fatal(err)
	}
	if o2.Base().Changeset != c.ID {
		t.Fatal("update did not move the base")
	}
	if b, err := o2.ReadFile(ctx, "README.md"); err != nil || string(b) != files["README.md"] {
		t.Fatalf("forced update should restore README.md: %q %v", b, err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "upper")); len(ents) != 0 {
		t.Fatalf("upper not emptied: %v", ents)
	}
	if _, err := o2.Stat(ctx, "other.txt"); err != nil {
		t.Fatal(fmt.Errorf("the new base's file: %w", err))
	}
}

// TestFindMountTrustsOnlyOwnFiles: a mount.json someone else owns, say
// planted in /tmp, must not redirect the kudzu command (and its token).
func TestFindMountTrustsOnlyOwnFiles(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, ".kudzu")
	os.MkdirAll(meta, 0o755)
	os.WriteFile(filepath.Join(meta, "mount.json"), []byte(`{"control": "/tmp/evil.sock", "remote": "evil.example:80"}`), 0o644)
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	m, ok := FindMount(filepath.Join(dir, "a", "b"))
	if !ok || m.Info.Root != dir || m.Info.Remote != "evil.example:80" {
		t.Fatalf("own mount.json not found: %+v %v", m, ok)
	}
	if os.Getuid() != 0 {
		t.Skip("changing a file's owner needs root")
	}
	if err := os.Chown(filepath.Join(meta, "mount.json"), 12345, 12345); err != nil {
		t.Skip(err)
	}
	if m, ok := FindMount(filepath.Join(dir, "a", "b")); ok {
		t.Fatalf("trusted a mount.json owned by someone else: %+v", m)
	}
}
