package vfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindMountTrustsOnlyPrivateMetadata: FindMount uses a mount.json only
// from the nearest .kudzu, and only if that is a real directory and the
// file a regular one, both the user's own and nobody else's to write.
// Anything else could have been planted, or redirected by a symlink, to
// send the kudzu command and its token elsewhere.
func TestFindMountTrustsOnlyPrivateMetadata(t *testing.T) {
	info := []byte(`{"control": "/tmp/kudzu-test/control.sock", "remote": "own.example:80"}`)
	// mount makes root/.kudzu/mount.json the way a mount shows it.
	mount := func(t *testing.T, root string) string {
		t.Helper()
		meta := filepath.Join(root, ".kudzu")
		if err := os.MkdirAll(meta, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(meta, "mount.json"), info, 0o444); err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
		return meta
	}
	found := func(t *testing.T, dir, root string) {
		t.Helper()
		if m, ok := FindMount(dir); !ok || m.Info.Root != root || m.Info.Remote != "own.example:80" {
			t.Fatalf("FindMount(%s) = %+v, %v; want the mount at %s", dir, m, ok, root)
		}
	}
	refused := func(t *testing.T, dir string) {
		t.Helper()
		if m, ok := FindMount(dir); ok {
			t.Fatalf("FindMount(%s) trusted %+v", dir, m.Info)
		}
	}

	t.Run("own", func(t *testing.T) {
		root := t.TempDir()
		mount(t, root)
		found(t, filepath.Join(root, "a", "b"), root)
	})
	t.Run("control socket without a path", func(t *testing.T) {
		// Nothing vouches for whoever bound an abstract socket, or for
		// where a relative path leads.
		root := t.TempDir()
		meta := mount(t, root)
		for _, ctl := range []string{"@kudzu-ctl", "control.sock"} {
			p := filepath.Join(meta, "mount.json")
			os.Remove(p)
			os.WriteFile(p, []byte(`{"control": "`+ctl+`", "remote": "own.example:80"}`), 0o444)
			refused(t, filepath.Join(root, "a", "b"))
		}
	})
	t.Run("nearer working copy", func(t *testing.T) {
		// A working copy below a mount.json, even the user's own, is not
		// inside that mount.
		root := t.TempDir()
		mount(t, root)
		wc := filepath.Join(root, "a")
		os.Mkdir(filepath.Join(wc, ".kudzu"), 0o755)
		refused(t, filepath.Join(wc, "b"))
	})
	t.Run("symlinked file", func(t *testing.T) {
		// A mount.json linking to some file of the user's, e.g. repository
		// content shaped like one: the link is not the user's to vouch for.
		root := t.TempDir()
		meta := mount(t, root)
		target := filepath.Join(root, "a", "fixture.json")
		os.WriteFile(target, info, 0o644)
		os.Remove(filepath.Join(meta, "mount.json"))
		if err := os.Symlink(target, filepath.Join(meta, "mount.json")); err != nil {
			t.Skip(err)
		}
		refused(t, filepath.Join(root, "a", "b"))
	})
	t.Run("symlinked directory", func(t *testing.T) {
		root, other := t.TempDir(), t.TempDir()
		mount(t, other)
		os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
		if err := os.Symlink(filepath.Join(other, ".kudzu"), filepath.Join(root, ".kudzu")); err != nil {
			t.Skip(err)
		}
		refused(t, filepath.Join(root, "a", "b"))
	})
	for _, mode := range []os.FileMode{0o775, 0o757, 0o777 | os.ModeSticky} {
		t.Run("writable by others "+mode.String(), func(t *testing.T) {
			root := t.TempDir()
			meta := mount(t, root)
			if err := os.Chmod(meta, mode); err != nil {
				t.Fatal(err)
			}
			refused(t, filepath.Join(root, "a", "b"))
		})
	}
	t.Run("file writable by others", func(t *testing.T) {
		root := t.TempDir()
		meta := mount(t, root)
		os.Chmod(filepath.Join(meta, "mount.json"), 0o666)
		refused(t, filepath.Join(root, "a", "b"))
	})
	t.Run("another user's directory", func(t *testing.T) {
		// Another user's .kudzu in a shared directory, holding a link to a
		// file of the user's, or a file of the user's they hard-linked.
		if os.Getuid() != 0 {
			t.Skip("changing a directory's owner needs root")
		}
		root := t.TempDir()
		meta := mount(t, root)
		target := filepath.Join(root, "a", "fixture.json")
		os.WriteFile(target, info, 0o644)
		os.Remove(filepath.Join(meta, "mount.json"))
		if err := os.Symlink(target, filepath.Join(meta, "mount.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(filepath.Join(meta, "mount.json"), 12345, 12345); err != nil {
			t.Skip(err)
		}
		if err := os.Chown(meta, 12345, 12345); err != nil {
			t.Skip(err)
		}
		refused(t, filepath.Join(root, "a", "b"))
		os.Remove(filepath.Join(meta, "mount.json"))
		if err := os.Link(target, filepath.Join(meta, "mount.json")); err != nil {
			t.Fatal(err)
		}
		refused(t, filepath.Join(root, "a", "b"))
	})
}
