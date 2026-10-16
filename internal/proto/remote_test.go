package proto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
)

// fetchServer starts a tier whose POST /objects/fetch answers with serve,
// and counts the requests it gets.
func fetchServer(t *testing.T, serve func(w http.ResponseWriter, ids []object.ID)) (*Remote, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /objects/fetch", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		b, _ := io.ReadAll(r.Body)
		ids, err := DecodeIDs(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serve(w, ids)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r, err := Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return r, &requests
}

// TestDialTimeouts: a tier that accepts connections but never answers must
// not hang its clients forever, but a slow land or rebase must still finish.
func TestDialTimeouts(t *testing.T) {
	r, err := Dial("127.0.0.1:7070")
	if err != nil {
		t.Fatal(err)
	}
	tr := r.Transport().(*http.Transport)
	if tr.DialContext == nil || tr.TLSHandshakeTimeout <= 0 {
		t.Fatal("no dial or TLS handshake timeout")
	}
	if d := tr.ResponseHeaderTimeout; d < 5*time.Minute {
		t.Fatalf("response header timeout %v: none, or too short for a server-side land", d)
	}
}

// TestFetchReturnsCallbackError: when fn fails for an object, Fetch fails,
// instead of dropping that object and succeeding with the rest on a retry.
func TestFetchReturnsCallbackError(t *testing.T) {
	good, goodStored := object.EncodeObject(object.KindBlob, []byte("good"))
	bad, _ := object.EncodeObject(object.KindBlob, []byte("bad"))
	corrupt := object.Encode(object.KindBlob, []byte("BAD"))
	r, _ := fetchServer(t, func(w http.ResponseWriter, ids []object.ID) {
		for _, id := range ids {
			if id == bad {
				WriteFrame(w, id, corrupt)
			} else {
				WriteFrame(w, id, goodStored)
			}
		}
	})
	errDisk := errors.New("disk full")
	for _, check := range []func(object.ID, []byte) error{
		func(id object.ID, b []byte) error { _, _, err := object.Verify(id, b); return err },
		func(id object.ID, b []byte) error { return errDisk },
	} {
		_, err := r.Fetch(context.Background(), []object.ID{bad, good}, check)
		if err == nil {
			t.Fatal("Fetch succeeded although fn failed")
		}
		if !strings.Contains(err.Error(), bad.Short()) && !errors.Is(err, errDisk) {
			t.Fatalf("error is not fn's: %v", err)
		}
	}
}

type sourceFunc func(ctx context.Context, ids []object.ID, emit func(object.ID, []byte) error) ([]object.ID, error)

func (f sourceFunc) FetchMany(ctx context.Context, ids []object.ID, emit func(object.ID, []byte) error) ([]object.ID, error) {
	return f(ctx, ids, emit)
}

// objectServer serves the object protocol from src.
func objectServer(t *testing.T, src Source) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterObjectRoutes(mux, src, &Counters{})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postFetch(t *testing.T, srv *httptest.Server, ids []object.ID) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/objects/fetch", "application/octet-stream", bytes.NewReader(EncodeIDs(ids)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestFetchStreamBreaksOnSourceError: when the source fails after some
// frames, the client gets those frames and then a broken stream, never a
// clean end it would take for the whole answer. Before any frame, it gets
// an error status.
func TestFetchStreamBreaksOnSourceError(t *testing.T) {
	a, sa := object.EncodeObject(object.KindBlob, []byte("a"))
	b, _ := object.EncodeObject(object.KindBlob, []byte("b"))
	fail := errors.New("disk on fire")
	srv := objectServer(t, sourceFunc(func(_ context.Context, ids []object.ID, emit func(object.ID, []byte) error) ([]object.ID, error) {
		if ids[0] == a {
			if err := emit(a, sa); err != nil {
				return nil, err
			}
		}
		return nil, fail
	}))

	fr := NewFrameReader(postFetch(t, srv, []object.ID{a, b}).Body)
	if id, data, _, err := fr.Next(); err != nil || id != a || !bytes.Equal(data, sa) {
		t.Fatalf("first frame: %s %v", id.Short(), err)
	}
	if _, _, _, err := fr.Next(); err == nil || err == io.EOF {
		t.Fatalf("after the source failed: %v, want a broken stream", err)
	}

	resp := postFetch(t, srv, []object.ID{b})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), fail.Error()) {
		t.Fatalf("failure before any frame: %d %q", resp.StatusCode, body)
	}
}

// TestFetchResumesAfterBrokenStream: the client keeps the frames that
// arrived before a stream broke and asks again for only the rest.
func TestFetchResumesAfterBrokenStream(t *testing.T) {
	objs := map[object.ID][]byte{}
	var ids []object.ID
	for _, s := range []string{"a", "b", "c"} {
		id, stored := object.EncodeObject(object.KindBlob, []byte(s))
		objs[id] = stored
		ids = append(ids, id)
	}
	var mu sync.Mutex
	var requests [][]object.ID
	srv := objectServer(t, sourceFunc(func(_ context.Context, want []object.ID, emit func(object.ID, []byte) error) ([]object.ID, error) {
		mu.Lock()
		requests = append(requests, want)
		first := len(requests) == 1
		mu.Unlock()
		for i, id := range want {
			if first && i == 1 {
				return nil, errors.New("upstream went away")
			}
			if err := emit(id, objs[id]); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}))
	r, err := Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got := map[object.ID]int{}
	missing, err := r.Fetch(context.Background(), ids, func(id object.ID, b []byte) error {
		got[id]++
		_, _, err := object.Verify(id, b)
		return err
	})
	if err != nil || len(missing) != 0 || len(got) != 3 {
		t.Fatalf("fetch: got %d objects, missing %d, err %v", len(got), len(missing), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || len(requests[1]) != 2 || requests[1][0] != ids[1] {
		t.Fatalf("requests: %v", requests)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("object %s delivered %d times", id.Short(), n)
		}
	}
}

// TestFetchTruncatedResponse: a response that ends cleanly without some of
// the objects is retried, then reported with a readable error.
func TestFetchTruncatedResponse(t *testing.T) {
	a, sa := object.EncodeObject(object.KindBlob, []byte("a"))
	b, _ := object.EncodeObject(object.KindBlob, []byte("b"))
	r, requests := fetchServer(t, func(w http.ResponseWriter, ids []object.ID) {
		for _, id := range ids {
			if id == a {
				WriteFrame(w, id, sa)
			}
		}
	})
	got := 0
	_, err := r.Fetch(context.Background(), []object.ID{a, b}, func(object.ID, []byte) error { got++; return nil })
	if err == nil || strings.Contains(err.Error(), "%!") || !strings.Contains(err.Error(), b.Short()) {
		t.Fatalf("error: %v", err)
	}
	if got != 1 || requests.Load() != fetchAttempts {
		t.Fatalf("delivered %d objects in %d requests", got, requests.Load())
	}
}
