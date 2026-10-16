package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
	"github.com/garyships/kudzu/internal/tier"
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

// brokenObjects fails to read one object, like a disk with a bad sector.
type brokenObjects struct {
	store.Objects
	broken object.ID
}

func (b *brokenObjects) Get(ctx context.Context, id object.ID) ([]byte, error) {
	if id == b.broken {
		return nil, errors.New("read error")
	}
	return b.Objects.Get(ctx, id)
}

// TestSnapshotFailureIsAnError: a snapshot that fails partway must not end
// like a complete one, directly or through a tier, so that seeding from it
// fails instead of starting a daemon on part of a revision. If the failure
// comes before anything was sent, the response is an error status; after
// that, the stream breaks.
func TestSnapshotFailureIsAnError(t *testing.T) {
	big := make([]byte, 256<<10) // more than the response buffer
	rand.Read(big)
	for _, first := range [][]byte{[]byte("a\n"), big} {
		ctx := t.Context()
		objs := &brokenObjects{Objects: store.NewMem(4 << 20)}
		r, srv := newTestServer(t, objs)
		var changes []object.Change
		for i, content := range [][]byte{first, []byte("b\n"), []byte("c\n")} {
			id, err := r.WriteBlob(ctx, content)
			if err != nil {
				t.Fatal(err)
			}
			changes = append(changes, object.Change{Path: string(rune('a' + i)), Mode: object.ModeFile, Blob: id})
		}
		c, err := r.Create(ctx, repo.CreateRequest{Author: "a <a@example.com>", Message: "m", Changes: changes})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Meta.CASRef(ctx, "main", object.Zero, c.ID); err != nil {
			t.Fatal(err)
		}
		objs.broken = changes[1].Blob

		up, err := proto.Dial(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		cache := httptest.NewServer(tier.New("cache", store.NewMem(1<<20), up, 4).Handler())
		defer cache.Close()
		for _, addr := range []string{srv.URL, cache.URL} {
			rem, err := proto.Dial(addr)
			if err != nil {
				t.Fatal(err)
			}
			body, err := rem.Stream(ctx, "/api/snapshot", url.Values{"rev": {"main"}})
			if err != nil {
				if len(first) == len(big) || !proto.IsStatus(err, http.StatusInternalServerError) {
					t.Fatalf("%d-byte first file, from %s: %v", len(first), addr, err)
				}
				continue
			}
			n, err := tier.New("daemon", store.NewMem(4<<20), rem, 4).Seed(body)
			body.Close()
			if err == nil {
				t.Fatalf("%d-byte first file, from %s: a snapshot with an unreadable object seeded %d objects without error", len(first), addr, n)
			}
		}
	}
}
