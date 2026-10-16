package tier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/store"
)

// upstream starts a fake tier whose POST /objects/fetch answers with serve.
func upstream(t *testing.T, serve func(w http.ResponseWriter, ids []object.ID)) *proto.Remote {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /objects/fetch", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ids, err := proto.DecodeIDs(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serve(w, ids)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	up, err := proto.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// TestCorruptUpstreamObjectNotCached: an object that arrives damaged from
// upstream fails that request and is not stored, so the tier serves the
// real object once upstream sends it intact.
func TestCorruptUpstreamObjectNotCached(t *testing.T) {
	ctx := context.Background()
	id, good := object.EncodeObject(object.KindBlob, []byte("hello"))
	bad := object.Encode(object.KindBlob, []byte("HELLO"))
	var corrupt atomic.Bool
	corrupt.Store(true)
	up := upstream(t, func(w http.ResponseWriter, ids []object.ID) {
		for _, x := range ids {
			if corrupt.Load() {
				proto.WriteFrame(w, x, bad)
			} else {
				proto.WriteFrame(w, x, good)
			}
		}
	})
	local := store.NewMem(1 << 20)
	tr := New("daemon", local, up, 2)
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	get := func() error {
		resp, err := http.Get(srv.URL + "/objects/" + id.String())
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d: %s", resp.StatusCode, b)
		}
		_, _, err = object.Verify(id, b)
		return err
	}

	if err := get(); err == nil {
		t.Fatal("corrupt upstream copy served as valid")
	}
	if ok, _ := local.Has(ctx, id); ok {
		t.Fatal("corrupt upstream copy stored")
	}
	if st := tr.Stats(); st.UpstreamFailures == 0 {
		t.Fatalf("corrupt copy not counted as an upstream failure: %+v", st)
	}
	corrupt.Store(false)
	if err := get(); err != nil {
		t.Fatalf("after upstream recovered: %v", err)
	}
	if ok, _ := local.Has(ctx, id); !ok {
		t.Fatal("intact copy not stored")
	}
}
