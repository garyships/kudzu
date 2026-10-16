package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/garyships/kudzu/internal/object"
)

// Remote talks to any tier over HTTP, or over a unix socket for addresses
// like unix:///run/kudzu.sock (how jobs reach their node's daemon).
type Remote struct {
	Addr string
	// Token is sent as a bearer token. Dial takes it from $KUDZU_TOKEN; it
	// is never written to disk.
	Token string
	base  string
	hc    *http.Client
}

func Dial(addr string) (*Remote, error) {
	if addr == "" {
		return nil, errors.New("no remote address (use --remote or KUDZU_REMOTE)")
	}
	tr := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // objects are already compressed
	}
	r := &Remote{Addr: addr, Token: os.Getenv("KUDZU_TOKEN"), hc: &http.Client{Transport: tr}}
	if sock, ok := strings.CutPrefix(addr, "unix://"); ok {
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		r.base = "http://unix"
		return r, nil
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	r.base = strings.TrimRight(addr, "/")
	return r, nil
}

// Transport exposes the underlying transport, e.g. for a reverse proxy.
func (r *Remote) Transport() http.RoundTripper { return r.hc.Transport }

func (r *Remote) BaseURL() *url.URL {
	u, _ := url.Parse(r.base)
	return u
}

// HTTPError is a non-2xx response. Conflicts lists paths for 409s; Path is
// the path or ref a 403 refers to.
type HTTPError struct {
	Status    int
	Message   string
	Conflicts []string
	Path      string
}

func (e *HTTPError) Error() string {
	if len(e.Conflicts) > 0 {
		return fmt.Sprintf("%s (conflicts: %s)", e.Message, strings.Join(e.Conflicts, ", "))
	}
	return e.Message
}

func IsStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

func (r *Remote) do(ctx context.Context, method, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := r.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		he := &HTTPError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
		var j struct {
			Error     string   `json:"error"`
			Conflicts []string `json:"conflicts"`
			Path      *string  `json:"path"`
		}
		if json.Unmarshal(b, &j) == nil && j.Error != "" {
			he.Message, he.Conflicts = j.Error, j.Conflicts
			if j.Path != nil {
				he.Path = *j.Path
			}
		}
		return nil, he
	}
	return resp, nil
}

func (r *Remote) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := r.do(ctx, "GET", path, q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Remote) PostJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := r.do(ctx, "POST", path, nil, bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Remote) GetRaw(ctx context.Context, path string, q url.Values) ([]byte, error) {
	resp, err := r.do(ctx, "GET", path, q, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Stream returns a response body for streaming endpoints (e.g. snapshots).
func (r *Remote) Stream(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	resp, err := r.do(ctx, "GET", path, q, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

const (
	fetchChunk       = 4096
	fetchParallelism = 4
	fetchAttempts    = 3
)

// Fetch requests objects in chunks, a few chunks at a time. fn is called once
// per received object (never concurrently). Interrupted chunks are retried for
// just the objects that did not arrive. It returns IDs no tier has. The first
// error, such as one returned by fn, stops the whole fetch and is returned.
func (r *Remote) Fetch(ctx context.Context, ids []object.ID, fn func(id object.ID, stored []byte) error) ([]object.ID, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var missing []object.ID
	var firstErr error
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	sem := make(chan struct{}, fetchParallelism)
	var wg sync.WaitGroup
	for start := 0; start < len(ids); start += fetchChunk {
		chunk := ids[start:min(start+fetchChunk, len(ids))]
		sem <- struct{}{}
		if failed() {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			miss, err := r.fetchChunk(ctx, chunk, func(id object.ID, b []byte) error {
				mu.Lock()
				defer mu.Unlock()
				return fn(id, b)
			})
			mu.Lock()
			defer mu.Unlock()
			missing = append(missing, miss...)
			if err != nil && firstErr == nil {
				firstErr = err
				cancel() // stop the other chunks too
			}
		}()
	}
	wg.Wait()
	return missing, firstErr
}

func (r *Remote) fetchChunk(ctx context.Context, ids []object.ID, fn func(object.ID, []byte) error) ([]object.ID, error) {
	pending := make(map[object.ID]bool, len(ids))
	for _, id := range ids {
		pending[id] = true
	}
	var missing []object.ID
	var lastErr error
	for attempt := 0; attempt < fetchAttempts && len(pending) > 0; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		want := make([]object.ID, 0, len(pending))
		for _, id := range ids {
			if pending[id] {
				want = append(want, id)
			}
		}
		var fnErr error
		lastErr = r.fetchOnce(ctx, want, func(id object.ID, b []byte, miss bool) error {
			if !pending[id] {
				return nil
			}
			if miss {
				delete(pending, id)
				missing = append(missing, id)
				return nil
			}
			// An object is done once fn has taken it. fn's error (a hash
			// mismatch, a full disk) is the caller's, not the stream's:
			// retrying would not help, and dropping it would turn into a
			// misleading "not found" later.
			if fnErr = fn(id, b); fnErr != nil {
				return fnErr
			}
			delete(pending, id)
			return nil
		})
		if fnErr != nil {
			return nil, fnErr
		}
		var he *HTTPError
		if errors.As(lastErr, &he) && he.Status < 500 {
			return nil, lastErr
		}
	}
	if len(pending) > 0 {
		if lastErr == nil {
			lastErr = errors.New("the response ended without them")
		}
		var first object.ID
		for _, id := range ids {
			if pending[id] {
				first = id
				break
			}
		}
		return missing, fmt.Errorf("fetch: %d objects not received (e.g. %s): %w", len(pending), first.Short(), lastErr)
	}
	return missing, nil
}

func (r *Remote) fetchOnce(ctx context.Context, ids []object.ID, fn func(object.ID, []byte, bool) error) error {
	resp, err := r.do(ctx, "POST", "/objects/fetch", nil, bytes.NewReader(EncodeIDs(ids)), "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fr := NewFrameReader(resp.Body)
	for {
		id, data, miss, err := fr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(id, data, miss); err != nil {
			return err
		}
	}
}

// Missing asks the server which of ids it does not have (used before upload).
func (r *Remote) Missing(ctx context.Context, ids []object.ID) ([]object.ID, error) {
	var out struct {
		Missing []object.ID `json:"missing"`
	}
	err := r.PostJSON(ctx, "/objects/missing", map[string]any{"ids": ids}, &out)
	return out.Missing, err
}

// Upload sends objects (stored form) to the server, which verifies each one.
func (r *Remote) Upload(ctx context.Context, objs map[object.ID][]byte) error {
	if len(objs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for id, b := range objs {
		WriteFrame(&buf, id, b)
	}
	resp, err := r.do(ctx, "POST", "/objects/upload", nil, &buf, "application/x-kudzu-frames")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
