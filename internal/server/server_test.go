package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

func newTestServer(t *testing.T, objs store.Objects) (*repo.Repo, *httptest.Server) {
	t.Helper()
	r := repo.New(storetest.Meta(t), objs)
	srv := httptest.NewServer(New(r, log.New(io.Discard, "", 0)).Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

// upload posts a frame stream and returns the status and how many bytes the
// whole process allocated meanwhile.
func upload(t *testing.T, srv *httptest.Server, body []byte) (int, uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, err := http.Post(srv.URL+"/objects/upload", "application/x-kudzu-frames", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	runtime.ReadMemStats(&after)
	return resp.StatusCode, after.TotalAlloc - before.TotalAlloc
}

// TestUploadClaimedLength: a frame header alone must not make the server
// allocate the length it claims.
func TestUploadClaimedLength(t *testing.T) {
	_, srv := newTestServer(t, store.NewMem(1<<20))
	var hdr [object.IDSize + 5]byte
	binary.BigEndian.PutUint32(hdr[object.IDSize+1:], 1<<30)
	status, alloc := upload(t, srv, hdr[:])
	if status != http.StatusBadRequest {
		t.Fatalf("status %d", status)
	}
	if alloc > 64<<20 {
		t.Fatalf("a 37-byte upload allocated %d MiB", alloc>>20)
	}
}

// TestUploadDecompressionBomb: a small blob that inflates to 32 MiB is
// verified without holding the content, whether its hash matches or not.
func TestUploadDecompressionBomb(t *testing.T) {
	objs := store.NewMem(1 << 20)
	_, srv := newTestServer(t, objs)
	content := make([]byte, 32<<20)
	id, stored := object.EncodeObject(object.KindBlob, content)
	content = nil
	for _, claim := range []object.ID{object.Hash(object.KindBlob, []byte("x")), id} {
		var body bytes.Buffer
		proto.WriteFrame(&body, claim, stored)
		status, alloc := upload(t, srv, body.Bytes())
		want := http.StatusBadRequest
		if claim == id {
			want = http.StatusOK
		}
		if status != want {
			t.Fatalf("upload as %s: status %d, want %d", claim.Short(), status, want)
		}
		if alloc > 8<<20 {
			t.Fatalf("a %d KiB upload allocated %d MiB", len(stored)>>10, alloc>>20)
		}
	}
	if ok, _ := objs.Has(t.Context(), id); !ok {
		t.Fatal("valid upload not stored")
	}
}
