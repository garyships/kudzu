package tier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/store"
)

// upstream starts a fake tier whose POST /objects/fetch answers with serve.
func upstream(t *testing.T, serve func(w http.ResponseWriter, r *http.Request, ids []object.ID)) *proto.Remote {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /objects/fetch", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ids, err := proto.DecodeIDs(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		serve(w, r, ids)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	up, err := proto.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return up
}

// TestUpstreamConcurrency: the upstream concurrency limit counts requests
// in flight, however the misses are batched. A batch larger than one fetch
// request used to run several requests at once under one slot.
func TestUpstreamConcurrency(t *testing.T) {
	const limit = 2
	var inflight, peak atomic.Int64
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request, ids []object.ID) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		for _, id := range ids {
			proto.WriteMissing(w, id)
		}
	})
	tr := New("cache", store.NewMem(1<<20), up, limit)
	var wg sync.WaitGroup
	for b := 0; b < 3; b++ {
		ids := make([]object.ID, 3*4096)
		for i := range ids {
			ids[i] = object.Hash(object.KindBlob, []byte(fmt.Sprint(b, i)))
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			missing, err := tr.FetchMany(context.Background(), ids, func(object.ID, []byte) error { return nil })
			if err != nil || len(missing) != len(ids) {
				t.Errorf("FetchMany: %d of %d missing, %v", len(missing), len(ids), err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > limit {
		t.Fatalf("%d upstream requests in flight with a limit of %d", p, limit)
	}
}

// TestRequestParallelism: one request's misses take at most
// upstreamParallelism upstream requests at a time, however many it has and
// however many upstream slots are free.
func TestRequestParallelism(t *testing.T) {
	var inflight, peak atomic.Int64
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request, ids []object.ID) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		for _, id := range ids {
			proto.WriteMissing(w, id)
		}
	})
	tr := New("cache", store.NewMem(1<<20), up, 32)
	ids := make([]object.ID, 3*upstreamParallelism*upstreamChunk)
	for i := range ids {
		ids[i] = object.Hash(object.KindBlob, []byte(fmt.Sprint(i)))
	}
	missing, err := tr.FetchMany(context.Background(), ids, func(object.ID, []byte) error { return nil })
	if err != nil || len(missing) != len(ids) {
		t.Fatalf("FetchMany: %d of %d missing, %v", len(missing), len(ids), err)
	}
	if p := peak.Load(); p > upstreamParallelism {
		t.Fatalf("one request had %d upstream requests in flight, want at most %d", p, upstreamParallelism)
	}
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
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request, ids []object.ID) {
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

// waitFor polls cond until it holds, failing the test after 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func inflight(tr *Tier) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return len(tr.inflight)
}

// TestAbandonedFetchCancelled: once no request waits for a fetch, every
// tier on the way drops it and cancels its upstream request. Fetches used
// to run on for minutes after their client hung up, so anyone could leave
// any number of them behind (random IDs, then hang up), starving every
// other miss and holding memory.
func TestAbandonedFetchCancelled(t *testing.T) {
	id, stored := object.EncodeObject(object.KindBlob, []byte("wanted later"))
	var block atomic.Bool
	block.Store(true)
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	stop := make(chan struct{})
	up := upstream(t, func(w http.ResponseWriter, r *http.Request, ids []object.ID) {
		if !block.Load() {
			for _, x := range ids {
				proto.WriteFrame(w, x, stored)
			}
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			select {
			case cancelled <- struct{}{}:
			default:
			}
		case <-stop:
		}
	})
	cache := New("cache", store.NewMem(1<<20), up, 2)
	cacheSrv := httptest.NewServer(cache.Handler())
	t.Cleanup(cacheSrv.Close)
	toCache, _ := proto.Dial(cacheSrv.URL)
	daemon := New("daemon", store.NewMem(1<<20), toCache, 2)
	daemonSrv := httptest.NewServer(daemon.Handler())
	t.Cleanup(daemonSrv.Close)
	t.Cleanup(func() { close(stop) }) // first, so the servers can close
	client, _ := proto.Dial(daemonSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		// Twice: each of a request's duplicates counts as a waiter.
		_, err := client.Fetch(ctx, []object.ID{id, id}, func(object.ID, []byte) error { return nil })
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the miss never reached upstream")
	}
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("fetch succeeded after its context was cancelled")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request still running after its only client hung up")
	}
	if n, m := inflight(daemon), inflight(cache); n != 0 || m != 0 {
		t.Fatalf("abandoned misses still in flight: daemon %d, cache %d", n, m)
	}

	// Nothing is left behind that would fail the next request for it.
	block.Store(false)
	var got []byte
	if _, err := client.Fetch(context.Background(), []object.ID{id}, func(_ object.ID, b []byte) error {
		got = b
		return nil
	}); err != nil || string(got) != string(stored) {
		t.Fatalf("fetch after the abandoned one: %q, %v", got, err)
	}
}

// TestFetchOutlivesFirstClient: a request joins the fetch already in flight
// for its object, and that fetch goes on for it when the request that
// started it hangs up.
func TestFetchOutlivesFirstClient(t *testing.T) {
	id, stored := object.EncodeObject(object.KindBlob, []byte("shared"))
	release := make(chan struct{})
	releaseNow := sync.OnceFunc(func() { close(release) })
	var requests atomic.Int64
	up := upstream(t, func(w http.ResponseWriter, r *http.Request, ids []object.ID) {
		requests.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		for _, x := range ids {
			proto.WriteFrame(w, x, stored)
		}
	})
	t.Cleanup(releaseNow) // before the upstream closes
	tr := New("cache", store.NewMem(1<<20), up, 2)
	nop := func(object.ID, []byte) error { return nil }

	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	go func() {
		_, err := tr.FetchMany(ctxA, []object.ID{id}, nop)
		errA <- err
	}()
	waitFor(t, "the first request's upstream fetch", func() bool { return requests.Load() == 1 })
	type result struct {
		data []byte
		err  error
	}
	resB := make(chan result, 1)
	go func() {
		var r result
		_, r.err = tr.FetchMany(context.Background(), []object.ID{id}, func(_ object.ID, b []byte) error {
			r.data = b
			return nil
		})
		resB <- r
	}()
	waitFor(t, "the second request to join", func() bool { return tr.Stats().Coalesced == 1 })

	cancelA()
	if err := <-errA; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request: %v", err)
	}
	releaseNow()
	select {
	case r := <-resB:
		if r.err != nil || string(r.data) != string(stored) {
			t.Fatalf("second request: %q, %v", r.data, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second request never answered")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d upstream requests for one object, want 1", n)
	}
}

// TestInflightCap: a tier keeps at most maxInflight misses in flight. A
// request with more waits for room instead of queueing them all, and one
// that gives up waiting leaves nothing behind.
func TestInflightCap(t *testing.T) {
	release := make(chan struct{})
	releaseNow := sync.OnceFunc(func() { close(release) })
	var tr *Tier
	var peak atomic.Int64
	started := make(chan struct{}, 1)
	up := upstream(t, func(w http.ResponseWriter, r *http.Request, ids []object.ID) {
		n := int64(inflight(tr))
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		for _, x := range ids {
			proto.WriteMissing(w, x)
		}
	})
	t.Cleanup(releaseNow) // before the upstream closes
	tr = New("cache", store.NewMem(1<<20), up, 2)
	tr.maxInflight = 4
	nop := func(object.ID, []byte) error { return nil }
	ids := make([]object.ID, 10)
	for i := range ids {
		ids[i] = object.Hash(object.KindBlob, []byte(fmt.Sprint(i)))
	}
	type result struct {
		missing []object.ID
		err     error
	}
	res := make(chan result, 1)
	go func() {
		var r result
		r.missing, r.err = tr.FetchMany(context.Background(), ids, nop)
		res <- r
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("no upstream request")
	}
	if n := inflight(tr); n != tr.maxInflight {
		t.Fatalf("%d misses in flight, want the cap of %d", n, tr.maxInflight)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	other := object.Hash(object.KindBlob, []byte("other"))
	if _, err := tr.FetchMany(gone, []object.ID{other}, nop); !errors.Is(err, context.Canceled) {
		t.Fatalf("request that gave up waiting for room: %v", err)
	}
	if n := inflight(tr); n != tr.maxInflight {
		t.Fatalf("%d misses in flight after a request gave up, want %d", n, tr.maxInflight)
	}

	releaseNow()
	select {
	case r := <-res:
		if r.err != nil || len(r.missing) != len(ids) {
			t.Fatalf("FetchMany: %d of %d missing, %v", len(r.missing), len(ids), r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request waiting for room never finished")
	}
	if p := peak.Load(); p > int64(tr.maxInflight) {
		t.Fatalf("%d misses in flight with a cap of %d", p, tr.maxInflight)
	}
	if n := inflight(tr); n != 0 {
		t.Fatalf("%d misses still in flight", n)
	}
}

// slowHas is a local store whose Has blocks for one object until released,
// like a stat on a stalled disk.
type slowHas struct {
	*store.Mem
	id      object.ID
	entered chan struct{}
	release chan struct{}
}

func (s *slowHas) Has(ctx context.Context, id object.ID) (bool, error) {
	if id == s.id {
		close(s.entered)
		<-s.release
	}
	return s.Mem.Has(ctx, id)
}

// TestSlowStatDoesNotStallOtherMisses: a tier checks its local store for
// new misses outside its lock, so a slow check (or 65536 of them in one
// request) does not hold up every other request's misses.
func TestSlowStatDoesNotStallOtherMisses(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request, ids []object.ID) {
		for _, x := range ids {
			proto.WriteMissing(w, x)
		}
	})
	x := object.Hash(object.KindBlob, []byte("x"))
	y := object.Hash(object.KindBlob, []byte("y"))
	local := &slowHas{Mem: store.NewMem(1 << 20), id: x, entered: make(chan struct{}), release: make(chan struct{})}
	releaseNow := sync.OnceFunc(func() { close(local.release) })
	t.Cleanup(releaseNow)
	tr := New("daemon", local, up, 2)
	nop := func(object.ID, []byte) error { return nil }

	errX := make(chan error, 1)
	go func() {
		_, err := tr.FetchMany(context.Background(), []object.ID{x}, nop)
		errX <- err
	}()
	<-local.entered
	errY := make(chan error, 1)
	go func() {
		missing, err := tr.FetchMany(context.Background(), []object.ID{y}, nop)
		if err == nil && len(missing) != 1 {
			err = fmt.Errorf("%d missing, want 1", len(missing))
		}
		errY <- err
	}()
	select {
	case err := <-errY:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a miss waited for another request's local store check")
	}
	releaseNow()
	if err := <-errX; err != nil {
		t.Fatal(err)
	}
}
