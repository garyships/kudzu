// Package server is the authoritative tier (scion-server in the article). It
// serves the full source control API (history, blame, diffs, ancestry,
// server-side commits, rebases, merges and lands) plus the object protocol.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
)

type Server struct {
	Repo  *repo.Repo
	Log   *log.Logger
	start time.Time

	counters proto.Counters
	created  atomic.Int64
	landed   atomic.Int64
	retries  atomic.Int64
}

func New(r *repo.Repo, logger *log.Logger) *Server {
	return &Server{Repo: r, Log: logger, start: time.Now()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	proto.RegisterObjectRoutes(mux, s, &s.counters)
	mux.HandleFunc("POST /objects/missing", s.handleMissing)
	mux.HandleFunc("POST /objects/upload", s.handleUpload)

	mux.HandleFunc("GET /api/resolve", s.handleResolve)
	mux.HandleFunc("GET /api/changesets/{id}", s.handleChangeset)
	mux.HandleFunc("POST /api/changesets", s.handleCreate)
	mux.HandleFunc("GET /api/log", s.handleLog)
	mux.HandleFunc("GET /api/blame", s.handleBlame)
	mux.HandleFunc("GET /api/diff", s.handleDiff)
	mux.HandleFunc("GET /api/cat", s.handleCat)
	mux.HandleFunc("GET /api/ls", s.handleLs)
	mux.HandleFunc("GET /api/ancestry", s.handleAncestry)
	mux.HandleFunc("GET /api/refs", s.handleRefs)
	mux.HandleFunc("POST /api/refs/{name...}", s.handleSetRef)
	mux.HandleFunc("POST /api/rebase", s.handleRebase)
	mux.HandleFunc("POST /api/merge", s.handleMerge)
	mux.HandleFunc("POST /api/land", s.handleLand)
	mux.HandleFunc("GET /api/wal", s.handleWAL)
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}

// FetchMany implements proto.Source straight from the object store.
func (s *Server) FetchMany(ctx context.Context, ids []object.ID, emit func(object.ID, []byte) error) ([]object.ID, error) {
	var missing []object.ID
	for _, id := range ids {
		b, err := s.Repo.Objs.Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			missing = append(missing, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := emit(id, b); err != nil {
			return nil, err
		}
	}
	return missing, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ce *repo.ConflictError
	switch {
	case errors.As(err, &ce):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflicts": ce.Paths})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, repo.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	default:
		s.Log.Printf("internal error: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

func (s *Server) resolve(ctx context.Context, rev string) (object.ID, error) {
	if rev == "" {
		rev = "main"
	}
	return s.Repo.Resolve(ctx, rev)
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<20)).Decode(v); err != nil {
		return fmt.Errorf("%w: bad request body: %v", repo.ErrInvalid, err)
	}
	return nil
}

func (s *Server) handleMissing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []object.ID `json:"ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	missing := []object.ID{}
	for _, id := range req.IDs {
		ok, err := s.Repo.Objs.Has(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !ok {
			missing = append(missing, id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	fr := proto.NewFrameReader(r.Body)
	stored := 0
	for {
		id, data, miss, err := fr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.fail(w, fmt.Errorf("%w: %v", repo.ErrInvalid, err))
			return
		}
		if miss {
			continue
		}
		kind, _, err := object.Verify(id, data)
		if err != nil {
			s.fail(w, fmt.Errorf("%w: %v", repo.ErrInvalid, err))
			return
		}
		if kind != object.KindBlob {
			// Trees and unodes are derived by the server, never uploaded.
			s.fail(w, fmt.Errorf("%w: cannot upload %s objects", repo.ErrInvalid, kind))
			return
		}
		if err := s.Repo.Objs.Put(r.Context(), id, data); err != nil {
			s.fail(w, err)
			return
		}
		stored++
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": stored})
}

type ResolveResponse struct {
	Changeset object.ID `json:"changeset"`
	Tree      object.ID `json:"tree"`
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.resolve(ctx, r.URL.Query().Get("rev"))
	if err != nil {
		s.fail(w, err)
		return
	}
	tree, err := s.Repo.TreeOf(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ResolveResponse{Changeset: id, Tree: tree})
}

// ChangesetView is a changeset as the API returns it.
type ChangesetView struct {
	ID   object.ID  `json:"id"`
	Seq  int64      `json:"seq,omitempty"`
	Tree *object.ID `json:"tree,omitempty"`
	*object.Changeset
	// Shadows the embedded field so summaries (log) omit the change list.
	Changes     []object.Change `json:"changes,omitempty"`
	ChangeCount int             `json:"change_count"`
}

func (s *Server) view(ctx context.Context, id object.ID, full bool) (*ChangesetView, error) {
	c, err := s.Repo.Changeset(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &ChangesetView{ID: id, Changeset: c, ChangeCount: len(c.Changes)}
	if full {
		_, seq, err := s.Repo.Meta.GetChangeset(ctx, id)
		if err != nil {
			return nil, err
		}
		tree, err := s.Repo.TreeOf(ctx, id)
		if err != nil {
			return nil, err
		}
		v.Seq, v.Tree, v.Changes = seq, &tree, c.Changes
		if v.Changes == nil {
			v.Changes = []object.Change{}
		}
	}
	return v, nil
}

func (s *Server) handleChangeset(w http.ResponseWriter, r *http.Request) {
	id, err := s.resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	v, err := s.view(r.Context(), id, true)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// CreateRequest optionally lands the new changeset in the same call.
type CreateRequest struct {
	repo.CreateRequest
	Land string `json:"land,omitempty"`
}

type CreateResponse struct {
	repo.Created
	Land *repo.LandResult `json:"land,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req CreateRequest
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	c, err := s.Repo.Create(ctx, req.CreateRequest)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.created.Add(1)
	resp := CreateResponse{Created: *c}
	if req.Land != "" {
		res, err := s.Repo.Land(ctx, c.ID, req.Land)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.noteLand(res)
		resp.Land = res
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) noteLand(res *repo.LandResult) {
	s.landed.Add(1)
	s.retries.Add(int64(res.Attempts - 1))
}

func limitParam(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		return min(n, 10000)
	}
	return def
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	head, err := s.resolve(ctx, r.URL.Query().Get("rev"))
	if err != nil {
		s.fail(w, err)
		return
	}
	limit := limitParam(r, 50)
	var ids []object.ID
	if p := strings.Trim(r.URL.Query().Get("path"), "/"); p != "" {
		ids, err = s.Repo.PathLog(ctx, head, p, limit)
	} else {
		ids, err = s.Repo.Log(ctx, head, limit)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]*ChangesetView, 0, len(ids))
	for _, id := range ids {
		v, err := s.view(ctx, id, false)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type BlameLine struct {
	repo.BlameLine
	Author string `json:"author"`
	Time   int64  `json:"time"`
}

func (s *Server) handleBlame(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	head, err := s.resolve(ctx, r.URL.Query().Get("rev"))
	if err != nil {
		s.fail(w, err)
		return
	}
	lines, err := s.Repo.Blame(ctx, head, strings.Trim(r.URL.Query().Get("path"), "/"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]BlameLine, len(lines))
	for i, l := range lines {
		c, err := s.Repo.Changeset(ctx, l.Changeset)
		if err != nil {
			s.fail(w, err)
			return
		}
		out[i] = BlameLine{BlameLine: l, Author: c.Author, Time: c.Time}
	}
	writeJSON(w, http.StatusOK, out)
}

type DiffResponse struct {
	From  object.ID       `json:"from"`
	To    object.ID       `json:"to"`
	Files []repo.FileDiff `json:"files"`
	Patch string          `json:"patch,omitempty"`
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	to, err := s.resolve(ctx, q.Get("to"))
	if err != nil {
		s.fail(w, err)
		return
	}
	var from object.ID
	if f := q.Get("from"); f != "" {
		if from, err = s.resolve(ctx, f); err != nil {
			s.fail(w, err)
			return
		}
	} else {
		c, err := s.Repo.Changeset(ctx, to)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(c.Parents) > 0 {
			from = c.Parents[0]
		}
	}
	if q.Get("merge_base") == "1" && !from.IsZero() {
		if from, err = s.Repo.MergeBase(ctx, from, to); err != nil {
			s.fail(w, err)
			return
		}
	}
	files, err := s.Repo.Diff(ctx, from, to, strings.Trim(q.Get("path"), "/"))
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := DiffResponse{From: from, To: to, Files: files}
	if resp.Files == nil {
		resp.Files = []repo.FileDiff{}
	}
	if q.Get("patch") == "1" {
		if resp.Patch, err = s.Repo.Patch(ctx, files); err != nil {
			s.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.resolve(ctx, r.URL.Query().Get("rev"))
	if err != nil {
		s.fail(w, err)
		return
	}
	e, b, err := s.Repo.ReadFile(ctx, id, strings.Trim(r.URL.Query().Get("path"), "/"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Kudzu-Mode", strconv.FormatUint(uint64(e.Mode), 8))
	w.Header().Set("X-Kudzu-Blob", e.ID.String())
	w.Write(b)
}

func (s *Server) handleLs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.resolve(ctx, r.URL.Query().Get("rev"))
	if err != nil {
		s.fail(w, err)
		return
	}
	entries, err := s.Repo.ListDir(ctx, id, strings.Trim(r.URL.Query().Get("path"), "/"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

type AncestryResponse struct {
	A           object.ID `json:"a"`
	B           object.ID `json:"b"`
	MergeBase   object.ID `json:"merge_base"`
	AIsAncestor bool      `json:"a_is_ancestor_of_b"`
	BIsAncestor bool      `json:"b_is_ancestor_of_a"`
}

func (s *Server) handleAncestry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var resp AncestryResponse
	var err error
	if resp.A, err = s.resolve(ctx, r.URL.Query().Get("a")); err != nil {
		s.fail(w, err)
		return
	}
	if resp.B, err = s.resolve(ctx, r.URL.Query().Get("b")); err != nil {
		s.fail(w, err)
		return
	}
	if resp.MergeBase, err = s.Repo.MergeBase(ctx, resp.A, resp.B); err != nil {
		s.fail(w, err)
		return
	}
	resp.AIsAncestor = resp.MergeBase == resp.A
	resp.BIsAncestor = resp.MergeBase == resp.B
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	refs, err := s.Repo.Meta.ListRefs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, refs)
}

func (s *Server) handleSetRef(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old object.ID `json:"old"`
		New string    `json:"new"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	var target object.ID
	if req.New != "" {
		var err error
		if target, err = s.Repo.Resolve(r.Context(), req.New); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.Repo.Meta.CASRef(r.Context(), r.PathValue("name"), req.Old, target); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": r.PathValue("name"), "target": target})
}

func (s *Server) handleRebase(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Head string `json:"head"`
		Onto string `json:"onto"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	head, err := s.resolve(ctx, req.Head)
	if err != nil {
		s.fail(w, err)
		return
	}
	onto, err := s.resolve(ctx, req.Onto)
	if err != nil {
		s.fail(w, err)
		return
	}
	newHead, err := s.Repo.Rebase(ctx, head, onto)
	if err != nil {
		s.fail(w, err)
		return
	}
	tree, err := s.Repo.TreeOf(ctx, newHead)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"head": newHead, "tree": tree})
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Ours    string `json:"ours"`
		Theirs  string `json:"theirs"`
		Author  string `json:"author"`
		Message string `json:"message"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	ours, err := s.resolve(ctx, req.Ours)
	if err != nil {
		s.fail(w, err)
		return
	}
	theirs, err := s.resolve(ctx, req.Theirs)
	if err != nil {
		s.fail(w, err)
		return
	}
	c, err := s.Repo.Merge(ctx, ours, theirs, req.Author, req.Message)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleLand(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Head string `json:"head"`
		Ref  string `json:"ref"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if req.Ref == "" {
		req.Ref = "main"
	}
	head, err := s.resolve(ctx, req.Head)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.Repo.Land(ctx, head, req.Ref)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.noteLand(res)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleWAL(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	entries, err := s.Repo.Meta.WAL(r.Context(), since, limitParam(r, 1000))
	if err != nil {
		s.fail(w, err)
		return
	}
	type walView struct {
		Seq int64     `json:"seq"`
		ID  object.ID `json:"id"`
		*object.Changeset
	}
	out := make([]walView, 0, len(entries))
	for _, e := range entries {
		c, err := object.DecodeChangeset(e.Raw)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, walView{e.Seq, e.ID, c})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSnapshot streams every tree and blob of one revision as frames. Node
// daemons start from such a snapshot and then fetch only what changed since.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.resolve(ctx, r.URL.Query().Get("rev"))
	if err != nil {
		s.fail(w, err)
		return
	}
	var dirs []string
	if sp := r.URL.Query().Get("sparse"); sp != "" {
		dirs = strings.Split(sp, ",")
	}
	spec, err := sparse.Normalize(dirs)
	if err != nil {
		s.fail(w, fmt.Errorf("%w: %v", repo.ErrInvalid, err))
		return
	}
	tree, err := s.Repo.TreeOf(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-kudzu-frames")
	w.Header().Set("X-Kudzu-Changeset", id.String())
	bw := bufio.NewWriterSize(w, 1<<16)
	err = s.Repo.WalkObjects(ctx, tree, spec, func(oid object.ID) error {
		b, err := s.Repo.Objs.Get(ctx, oid)
		if err != nil {
			return err
		}
		return proto.WriteFrame(bw, oid, b)
	})
	if err != nil {
		s.Log.Printf("snapshot: %v", err)
	}
	bw.Flush()
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tier":               "server",
		"uptime_seconds":     int(time.Since(s.start).Seconds()),
		"requests":           s.counters.Requests.Load(),
		"objects_served":     s.counters.ObjectsServed.Load(),
		"bytes_served":       s.counters.BytesServed.Load(),
		"changesets_created": s.created.Load(),
		"lands":              s.landed.Load(),
		"land_retries":       s.retries.Load(),
	})
}
