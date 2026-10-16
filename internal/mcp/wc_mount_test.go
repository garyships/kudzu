package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/vfs"
)

// TestWorkingCopyToolsInMount: the working copy tools act on a writable
// mount's overlay when dir is inside one. The mount itself needs FUSE, so
// the test serves an overlay's control socket and writes the mount.json a
// mount would show.
func TestWorkingCopyToolsInMount(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ovDir, err := os.MkdirTemp("", "kzm") // socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ovDir)
	o, err := vfs.OpenOverlay(ctx, ovDir, e.remote, "main", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go o.ServeControl(ctx, nil)
	f, err := o.OpenWrite(ctx, "services/api/new.go", true, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("package main\n")
	o.CloseWrite(f)

	mnt := t.TempDir()
	os.MkdirAll(filepath.Join(mnt, ".kudzu"), 0o755)
	info, _ := json.Marshal(vfs.MountInfo{Control: vfs.ControlSocket(ovDir), Remote: e.url})
	os.WriteFile(filepath.Join(mnt, ".kudzu", "mount.json"), info, 0o644)
	os.MkdirAll(filepath.Join(mnt, "services", "api"), 0o755)
	for i := 0; i < 200; i++ { // wait for the control socket
		if _, err := os.Stat(vfs.ControlSocket(ovDir)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cs := e.connect(t, Options{Author: "agent <a@example.com>"})
	var st struct {
		Mount bool
		Files []struct{ Path, Status string }
	}
	ok(t, cs, "kudzu_wc_status", map[string]any{"dir": filepath.Join(mnt, "services", "api")}, &st)
	if !st.Mount || len(st.Files) != 1 || st.Files[0].Path != "services/api/new.go" || st.Files[0].Status != "A" {
		t.Fatalf("status = %+v", st)
	}
	var res struct {
		Changeset string
		NowAt     string `json:"now_at"`
		Changes   int
		Land      struct{ Landed string }
	}
	ok(t, cs, "kudzu_wc_commit", map[string]any{"dir": mnt, "message": "from a mount", "land": "main"}, &res)
	tip, _ := e.repo.Meta.GetRef(ctx, "main")
	if res.Changes != 1 || res.Land.Landed != tip.String() || res.NowAt != tip.String() || o.Base().Changeset != tip {
		t.Fatalf("commit = %+v, main = %s", res, tip.Short())
	}
	if msg := fails(t, cs, "kudzu_wc_commit", map[string]any{"dir": mnt, "message": "again"}); !strings.Contains(msg, "nothing to commit") {
		t.Fatalf("empty commit: %s", msg)
	}
	var upd struct{ Previous, Changeset string }
	ok(t, cs, "kudzu_wc_update", map[string]any{"dir": mnt, "rev": "main~1"}, &upd)
	if upd.Previous != tip.String() || upd.Changeset == tip.String() {
		t.Fatalf("update = %+v", upd)
	}
}
