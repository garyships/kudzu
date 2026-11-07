// Package tier implements the caching tiers between clients and the server:
// a regional cache (scion-cache: objects in memory) and a per-node daemon
// (scion-daemon: objects on local disk, shared by every job on the machine).
// Both answer exactly the same object RPCs as the server, and forward
// everything else upstream, so tiers stack without clients noticing.
//
// Tiers isolate failure domains: concurrent misses for one object collapse
// into a single upstream request, and the number of in-flight upstream
// requests is capped, so a cold or restarted cache cannot stampede the tier
// above it.
package tier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/store"
)

type Tier struct {
	Name  string
	Local store.Objects
	Up    *proto.Remote

	mu          sync.Mutex
	inflight    map[object.ID]*call
	maxInflight int
	room        chan struct{} // closed once inflight has room again; nil if nobody waits
	start       time.Time

	counters   proto.Counters
	hits       atomic.Int64
	misses     atomic.Int64
	coalesced  atomic.Int64
	upRequests atomic.Int64
	upObjects  atomic.Int64
	upBytes    atomic.Int64
	upFailures atomic.Int64
}

// A request's misses go upstream at most upstreamChunk per request and
// upstreamParallelism requests at a time, as proto.Remote.Fetch sends them,
// so one large request can't take every upstream slot or pile up answers.
const (
	upstreamChunk       = 4096
	upstreamParallelism = 4
)

// A call is the fetch of one object for the requests waiting for it.
type call struct {
	id      object.ID
	done    chan struct{} // closed under Tier.mu once the result is set
	waiters int           // guarded by Tier.mu; 0 once abandoned
	job     *job          // guarded by Tier.mu
	data    []byte
	missing bool
	err     error
}

// A job is one upstream request.
type job struct {
	live   int // its calls not done and still waited for; guarded by Tier.mu
	cancel context.CancelFunc
}

// New returns a tier that fetches misses through up, with at most
// upstreamConcurrency requests to it in flight (it sets up's fetch limit).
func New(name string, local store.Objects, up *proto.Remote, upstreamConcurrency int) *Tier {
	if upstreamConcurrency <= 0 {
		upstreamConcurrency = 16
	}
	// The cap is on requests, not on batches of misses: one batch can span
	// several requests, which Fetch runs in parallel.
	up.SetFetchLimit(upstreamConcurrency)
	return &Tier{
		Name:     name,
		Local:    local,
		Up:       up,
		inflight: map[object.ID]*call{},
		// Enough to fill every upstream request in flight and as many again
		// waiting for a slot; more would only queue, holding memory.
		maxInflight: 2 * upstreamConcurrency * upstreamChunk,
		start:       time.Now(),
	}
}

// FetchMany serves hits from the local store immediately and fetches misses
// upstream, joining any fetch already in flight for the same object.
func (t *Tier) FetchMany(ctx context.Context, ids []object.ID, emit func(object.ID, []byte) error) ([]object.ID, error) {
	var misses []object.ID
	for _, id := range ids {
		b, err := t.Local.Get(ctx, id)
		if err == nil {
			t.hits.Add(1)
			if err := emit(id, b); err != nil {
				return nil, err
			}
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		misses = append(misses, id)
	}
	if len(misses) == 0 {
		return nil, nil
	}
	t.misses.Add(int64(len(misses)))

	calls := make([]*call, len(misses))
	// What this request stops waiting for early (it hung up or failed) may
	// be wanted by nobody else, and is then no longer fetched.
	pending := calls
	defer func() { t.release(pending) }()
	var missing []object.ID
	answered := 0
	answer := func(upTo int) error {
		for ; answered < upTo; answered++ {
			c := calls[answered]
			select {
			case <-c.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			pending = calls[answered+1:]
			if c.err != nil {
				return c.err
			}
			if c.missing {
				missing = append(missing, misses[answered])
			} else if err := emit(misses[answered], c.data); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < len(misses); {
		// At most upstreamParallelism requests' worth of misses waiting.
		if err := answer(i - (upstreamParallelism-1)*upstreamChunk); err != nil {
			return nil, err
		}
		var err error
		if i, err = t.join(ctx, misses, calls, i); err != nil {
			return nil, err
		}
	}
	if err := answer(len(misses)); err != nil {
		return nil, err
	}
	return missing, nil
}

// join makes the request a waiter on a call for each of ids[i:], up to one
// upstream request's worth, joining the call in flight or starting a new
// one, and returns where it stopped. While maxInflight misses are in
// flight, it waits for room rather than add more.
func (t *Tier) join(ctx context.Context, ids []object.ID, calls []*call, i int) (int, error) {
	end := min(len(ids), i+upstreamChunk)
	var mine []*call
	var room chan struct{}
	t.mu.Lock()
	for ; i < end; i++ {
		if c, ok := t.inflight[ids[i]]; ok {
			c.waiters++
			calls[i] = c
			t.coalesced.Add(1)
			continue
		}
		if len(t.inflight) >= t.maxInflight {
			if t.room == nil {
				t.room = make(chan struct{})
			}
			room = t.room
			break
		}
		c := &call{id: ids[i], done: make(chan struct{}), waiters: 1}
		t.inflight[c.id] = c
		calls[i] = c
		mine = append(mine, c)
	}
	t.mu.Unlock()
	t.launch(ctx, mine)
	if room != nil {
		select {
		case <-room:
		case <-ctx.Done():
			return i, ctx.Err()
		}
	}
	return i, nil
}

// launch fetches calls upstream, except objects stored since the request
// missed them: a fetch may have finished in between (fetchUpstream stores
// before it completes). This runs outside t.mu, as each check can be a file
// system stat.
func (t *Tier) launch(ctx context.Context, calls []*call) {
	var fetch []*call
	for _, c := range calls {
		if ok, _ := t.Local.Has(ctx, c.id); ok {
			if b, err := t.Local.Get(ctx, c.id); err == nil {
				t.coalesced.Add(1)
				t.complete(c, b, false, nil)
				continue
			}
		}
		fetch = append(fetch, c)
	}
	if len(fetch) > 0 {
		// Detached from this request: other requests may be waiting on it.
		go t.fetchUpstream(fetch)
	}
}

// release stops the request waiting for calls. A call nobody waits for any
// more leaves inflight, so the next request for its object starts afresh,
// and an upstream request none of whose objects is still wanted is
// cancelled.
func (t *Tier) release(calls []*call) {
	if len(calls) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range calls {
		if c == nil {
			continue // never joined: the request gave up waiting for room
		}
		select {
		case <-c.done:
			continue
		default:
		}
		if c.waiters--; c.waiters > 0 {
			continue
		}
		t.remove(c)
		if j := c.job; j != nil {
			if j.live--; j.live == 0 {
				j.cancel()
			}
		}
	}
}

// remove takes c out of inflight; t.mu is held.
func (t *Tier) remove(c *call) {
	delete(t.inflight, c.id)
	// Wake requests waiting for room once there is a fair amount of it,
	// not on every object.
	if t.room != nil && len(t.inflight) <= t.maxInflight-t.maxInflight/4 {
		close(t.room)
		t.room = nil
	}
}

// fetchUpstream fetches calls, at most upstreamChunk of them, in one
// upstream request. It runs detached from the requests waiting for them, so
// one client hanging up doesn't fail the others, until none of them is
// wanted any more.
func (t *Tier) fetchUpstream(calls []*call) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	j := &job{cancel: cancel}
	ids := make([]object.ID, 0, len(calls))
	byID := make(map[object.ID]*call, len(calls))
	t.mu.Lock()
	for _, c := range calls {
		if c.waiters == 0 {
			continue // abandoned already
		}
		c.job = j
		j.live++
		ids = append(ids, c.id)
		byID[c.id] = c
	}
	t.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	t.upRequests.Add(1)
	missing, err := t.Up.Fetch(ctx, ids, func(id object.ID, b []byte) error {
		c := byID[id]
		if c == nil {
			return nil
		}
		delete(byID, id)
		t.upObjects.Add(1)
		t.upBytes.Add(int64(len(b)))
		// Whatever is stored here is served to every client from now on,
		// so a copy damaged on the way fails this fetch instead.
		if _, err := object.Check(id, b); err != nil {
			t.upFailures.Add(1)
			t.complete(c, nil, false, fmt.Errorf("from upstream %s: %w", t.Up.Addr, err))
			return nil
		}
		t.Local.Put(ctx, id, b)
		t.complete(c, b, false, nil)
		return nil
	})
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		t.upFailures.Add(1) // but not when cancelled for want of waiters
	}
	for _, id := range missing {
		if c := byID[id]; c != nil {
			delete(byID, id)
			t.complete(c, nil, true, nil)
		}
	}
	if err == nil {
		err = errors.New("upstream did not return object")
	}
	for _, c := range byID {
		t.complete(c, nil, false, err)
	}
}

// complete settles c for every request waiting for it. An abandoned call is
// left alone: inflight may hold a newer call for its object by now.
func (t *Tier) complete(c *call, data []byte, missing bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.waiters == 0 {
		return
	}
	t.remove(c)
	if c.job != nil {
		c.job.live--
	}
	c.data, c.missing, c.err = data, missing, err
	close(c.done)
}

// Seed loads a snapshot (a stream of frames) into the local store.
func (t *Tier) Seed(r io.Reader) (int, error) {
	fr := proto.NewFrameReader(r)
	n := 0
	for {
		id, data, miss, err := fr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if miss {
			continue
		}
		if _, err := object.Check(id, data); err != nil {
			return n, err
		}
		if err := t.Local.Put(context.Background(), id, data); err != nil {
			return n, err
		}
		n++
	}
}

type Stats struct {
	Tier             string  `json:"tier"`
	UptimeSeconds    int     `json:"uptime_seconds"`
	Requests         int64   `json:"requests"`
	Hits             int64   `json:"hits"`
	Misses           int64   `json:"misses"`
	HitRate          float64 `json:"hit_rate"`
	Coalesced        int64   `json:"coalesced_misses"`
	UpstreamRequests int64   `json:"upstream_requests"`
	UpstreamObjects  int64   `json:"upstream_objects"`
	UpstreamBytes    int64   `json:"upstream_bytes"`
	UpstreamFailures int64   `json:"upstream_failures"`
	ObjectsServed    int64   `json:"objects_served"`
	BytesServed      int64   `json:"bytes_served"`
	CachedObjects    int     `json:"cached_objects,omitempty"`
	CachedBytes      int64   `json:"cached_bytes,omitempty"`
}

func (t *Tier) Stats() Stats {
	s := Stats{
		Tier:             t.Name,
		UptimeSeconds:    int(time.Since(t.start).Seconds()),
		Requests:         t.counters.Requests.Load(),
		Hits:             t.hits.Load(),
		Misses:           t.misses.Load(),
		Coalesced:        t.coalesced.Load(),
		UpstreamRequests: t.upRequests.Load(),
		UpstreamObjects:  t.upObjects.Load(),
		UpstreamBytes:    t.upBytes.Load(),
		UpstreamFailures: t.upFailures.Load(),
		ObjectsServed:    t.counters.ObjectsServed.Load(),
		BytesServed:      t.counters.BytesServed.Load(),
	}
	if total := s.Hits + s.Misses; total > 0 {
		s.HitRate = float64(s.Hits) / float64(total)
	}
	switch l := t.Local.(type) {
	case *store.Mem:
		s.CachedObjects, s.CachedBytes = l.Stats()
	case *store.FS:
		s.CachedBytes = l.Size()
	}
	return s
}

// Handler serves the object protocol locally and proxies the rest upstream.
func (t *Tier) Handler() http.Handler {
	mux := http.NewServeMux()
	proto.RegisterObjectRoutes(mux, t, &t.counters)
	proxy := httputil.NewSingleHostReverseProxy(t.Up.BaseURL())
	proxy.Transport = t.Up.Transport()
	proxy.FlushInterval = -1
	mux.Handle("/api/", proxy)
	mux.Handle("POST /objects/missing", proxy)
	mux.Handle("POST /objects/upload", proxy)
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t.Stats())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}
