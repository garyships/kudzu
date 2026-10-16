package proto

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
