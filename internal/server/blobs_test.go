package server

import (
	"bytes"
	"io"
	"net/url"
	"runtime"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
)

// TestCatAndSizesDoNotInflateIntoMemory: a small stored blob that inflates
// to 32 MiB is served by /api/cat and measured by /api/sizes without the
// server holding its content. A blob found corrupt after part of it was
// served breaks the response instead of ending it like a whole file.
func TestCatAndSizesDoNotInflateIntoMemory(t *testing.T) {
	ctx := t.Context()
	objs := store.NewMem(4 << 20)
	r, srv := newTestServer(t, objs)
	big, bigStored := object.EncodeObject(object.KindBlob, make([]byte, 32<<20))
	corrupt, corruptStored := object.EncodeObject(object.KindBlob, bytes.Repeat([]byte("kudzu\n"), 200<<10))
	corruptStored[len(corruptStored)-1] ^= 1 // the zlib checksum, checked at the end
	var changes []object.Change
	for path, b := range map[string]struct {
		id     object.ID
		stored []byte
	}{"big": {big, bigStored}, "bad/corrupt": {corrupt, corruptStored}} {
		if err := objs.Put(ctx, b.id, b.stored); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, object.Change{Path: path, Mode: object.ModeFile, Blob: b.id})
	}
	c, err := r.Create(ctx, repo.CreateRequest{Author: "a <a@example.com>", Message: "m", Changes: changes})
	if err != nil {
		t.Fatal(err)
	}
	rem, err := proto.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	measure := func(what string, f func()) {
		t.Helper()
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
			t.Fatalf("%s of a %d KiB stored blob allocated %d MiB", what, len(bigStored)>>10, n>>20)
		}
	}
	measure("sizes", func() {
		var sizes map[string]int64
		if err := rem.GetJSON(ctx, "/api/sizes", url.Values{"tree": {c.Tree.String()}}, &sizes); err != nil {
			t.Fatal(err)
		}
		if sizes["big"] != 32<<20 {
			t.Fatalf("sizes = %v", sizes)
		}
	})
	measure("cat", func() {
		rc, err := rem.Stream(ctx, "/api/cat", url.Values{"rev": {c.ID.String()}, "path": {"big"}})
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		if n, err := io.Copy(io.Discard, rc); err != nil || n != 32<<20 {
			t.Fatalf("cat read %d bytes: %v", n, err)
		}
	})
	rc, err := rem.Stream(ctx, "/api/cat", url.Values{"rev": {c.ID.String()}, "path": {"bad/corrupt"}})
	if err == nil {
		_, err = io.Copy(io.Discard, rc)
		rc.Close()
	}
	if err == nil {
		t.Fatal("cat of a corrupt blob ended like a whole file")
	}
}
