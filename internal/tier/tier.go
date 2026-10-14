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

	sem      chan struct{}
	mu       sync.Mutex
	inflight map[object.ID]*call
	start    time.Time

	counters   proto.Counters
	hits       atomic.Int64
	misses     atomic.Int64
	coalesced  atomic.Int64
	upRequests atomic.Int64
	upObjects  atomic.Int64
	upBytes    atomic.Int64
	upFailures atomic.Int64
}

type call struct {
	done    chan struct{}
	data    []byte
	missing bool
	err     error
}

func New(name string, local store.Objects, up *proto.Remote, upstreamConcurrency int) *Tier {
	if upstreamConcurrency <= 0 {
		upstreamConcurrency = 16
	}
	return &Tier{
		Name:     name,
		Local:    local,
		Up:       up,
		sem:      make(chan struct{}, upstreamConcurrency),
		inflight: map[object.ID]*call{},
		start:    time.Now(),
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
	var mine []object.ID
	var late []object.ID
	t.mu.Lock()
	for i, id := range misses {
		if c, ok := t.inflight[id]; ok {
			calls[i] = c
			t.coalesced.Add(1)
			continue
		}
		// A fetch may have finished between our lookup and taking the lock;
		// fetchUpstream stores before it clears inflight, so check again.
		if ok, _ := t.Local.Has(ctx, id); ok {
			late = append(late, id)
			continue
		}
		c := &call{done: make(chan struct{})}
		t.inflight[id] = c
		calls[i] = c
		mine = append(mine, id)
	}
	t.mu.Unlock()
	var missing []object.ID
	for _, id := range late {
		b, err := t.Local.Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			// Evicted again in between (memory pressure): start over for it.
			m, err := t.FetchMany(ctx, []object.ID{id}, emit)
			if err != nil {
				return nil, err
			}
			missing = append(missing, m...)
			continue
		}
		if err != nil {
			return nil, err
		}
		t.coalesced.Add(1)
		if err := emit(id, b); err != nil {
			return nil, err
		}
	}
	if len(mine) > 0 {
		// Detached from this request: other requests may be waiting on it.
		go t.fetchUpstream(mine)
	}

	for i, c := range calls {
		if c == nil {
			continue // served above
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c.err != nil {
			return nil, c.err
		}
		if c.missing {
			missing = append(missing, misses[i])
			continue
		}
		if err := emit(misses[i], c.data); err != nil {
			return nil, err
		}
	}
	return missing, nil
}

func (t *Tier) fetchUpstream(ids []object.ID) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.sem <- struct{}{}
	defer func() { <-t.sem }()
	t.upRequests.Add(1)
	got := make(map[object.ID]bool, len(ids))
	missing, err := t.Up.Fetch(ctx, ids, func(id object.ID, b []byte) error {
		t.upObjects.Add(1)
		t.upBytes.Add(int64(len(b)))
		t.Local.Put(ctx, id, b)
		got[id] = true
		t.complete(id, b, false, nil)
		return nil
	})
	if err != nil {
		t.upFailures.Add(1)
	}
	isMissing := make(map[object.ID]bool, len(missing))
	for _, id := range missing {
		isMissing[id] = true
	}
	for _, id := range ids {
		if got[id] {
			continue
		}
		if isMissing[id] {
			t.complete(id, nil, true, nil)
		} else {
			if err == nil {
				err = errors.New("upstream did not return object")
			}
			t.complete(id, nil, false, err)
		}
	}
}

func (t *Tier) complete(id object.ID, data []byte, missing bool, err error) {
	t.mu.Lock()
	c := t.inflight[id]
	delete(t.inflight, id)
	t.mu.Unlock()
	if c != nil {
		c.data, c.missing, c.err = data, missing, err
		close(c.done)
	}
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
		if _, _, err := object.Verify(id, data); err != nil {
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
