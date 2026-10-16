package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
)

// A writable mount is controlled through a unix socket in its overlay
// directory, so that kudzu status, diff, commit, update and land work from
// inside the mount by asking the mount process, which owns the overlay:
//
//	GET  /base                            {"changeset"}
//	GET  /status                          {"changeset", "files": [{"path", "status"}]}
//	GET  /diff                            {"files", "patch"}
//	POST /commit {"message","author","land"}  CommitResult
//	POST /update {"rev","force"}          {"changeset"}
//	POST /land   {"ref"}                  repo.LandResult
//
// The mount shows where the socket is in a read-only file at
// .kudzu/mount.json in its root (.kudzu is reserved, so no repository can
// have that path).

// MountInfo is the content of .kudzu/mount.json.
type MountInfo struct {
	Control string `json:"control"` // unix socket path
	Remote  string `json:"remote"`
	Root    string `json:"-"` // the mountpoint, filled in by FindMount
}

// MountInfoPath is where a mount shows its MountInfo, relative to its root.
const MountInfoPath = ".kudzu/mount.json"

// ControlSocket is the control socket's path for the overlay in dir.
func ControlSocket(dir string) string { return filepath.Join(dir, "control.sock") }

// StatusResponse is GET /status.
type StatusResponse struct {
	Changeset object.ID    `json:"changeset"`
	Files     []FileStatus `json:"files"`
}

// ServeControl serves the control API until ctx ends. onReset runs after
// the overlay moves to another base (commit, update, land), so a
// filesystem can drop what it cached about the old one.
func (o *Overlay) ServeControl(ctx context.Context, onReset func()) error {
	sock := ControlSocket(o.dir)
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)
	reset := func() {
		if onReset != nil {
			onReset()
		}
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			status := http.StatusInternalServerError
			var he *proto.HTTPError
			switch {
			case errors.As(err, &he):
				status = he.Status
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"error": he.Message, "conflicts": he.Conflicts})
				return
			case errors.Is(err, ErrNothingToCommit):
				status = http.StatusBadRequest
			case errors.Is(err, errDirty):
				status = http.StatusConflict
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	decode := func(r *http.Request, v any) error {
		return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
	}
	mux.HandleFunc("GET /base", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"changeset": o.Base().Changeset}, nil)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		files, err := o.Changes(r.Context())
		if files == nil {
			files = []FileStatus{}
		}
		reply(w, StatusResponse{Changeset: o.Base().Changeset, Files: files}, err)
	})
	mux.HandleFunc("GET /diff", func(w http.ResponseWriter, r *http.Request) {
		files, patch, err := o.Diff(r.Context())
		if files == nil {
			files = []FileStatus{}
		}
		reply(w, map[string]any{"files": files, "patch": patch}, err)
	})
	mux.HandleFunc("POST /commit", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Message, Author, Land string }
		if err := decode(r, &req); err != nil {
			reply(w, nil, err)
			return
		}
		res, err := o.Commit(r.Context(), req.Message, req.Author, req.Land)
		if err == nil {
			reset()
		}
		reply(w, res, err)
	})
	mux.HandleFunc("POST /update", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rev   string
			Force bool
		}
		if err := decode(r, &req); err != nil {
			reply(w, nil, err)
			return
		}
		err := o.Update(r.Context(), req.Rev, req.Force)
		if err == nil {
			reset()
		}
		reply(w, map[string]any{"changeset": o.Base().Changeset}, err)
	})
	mux.HandleFunc("POST /land", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Ref string }
		if err := decode(r, &req); err != nil {
			reply(w, nil, err)
			return
		}
		res, err := o.Land(r.Context(), req.Ref)
		if err == nil {
			reset()
		}
		reply(w, res, err)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
		os.Remove(sock)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

var errDirty = errors.New("the mount has uncommitted changes")

// Land lands the overlay's base on ref and moves the overlay there. The
// overlay must be clean: moving uncommitted changes onto other people's
// work could silently drop theirs.
func (o *Overlay) Land(ctx context.Context, ref string) (*repo.LandResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	changes, err := o.changes(ctx)
	if err != nil {
		return nil, err
	}
	if len(changes) > 0 {
		return nil, fmt.Errorf("%w (%d); commit them with kudzu commit --land %s instead", errDirty, len(changes), ref)
	}
	var res repo.LandResult
	if err := o.remote.PostJSON(ctx, "/api/land", map[string]string{"head": o.base.Changeset.String(), "ref": ref}, &res); err != nil {
		return nil, err
	}
	if res.Landed != o.base.Changeset {
		if err := o.reset(ctx, res.Landed.String()); err != nil {
			return nil, err
		}
	}
	return &res, nil
}

// MountClient talks to a writable mount's control socket.
type MountClient struct {
	Info MountInfo
	ctl  *proto.Remote
}

// FindMount finds the writable mount containing dir, if any.
func FindMount(dir string) (*MountClient, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, false
	}
	for d := abs; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, filepath.FromSlash(MountInfoPath))); err == nil {
			var info MountInfo
			if json.Unmarshal(b, &info) != nil || info.Control == "" {
				return nil, false
			}
			info.Root = d
			ctl, err := proto.Dial("unix://" + info.Control)
			if err != nil {
				return nil, false
			}
			ctl.Token = "" // the control socket is private to the mount's owner
			return &MountClient{Info: info, ctl: ctl}, true
		}
		if filepath.Dir(d) == d {
			return nil, false
		}
	}
}

// Base returns the changeset the mount's overlay sits on.
func (m *MountClient) Base(ctx context.Context) (object.ID, error) {
	var res struct {
		Changeset object.ID `json:"changeset"`
	}
	err := m.ctl.GetJSON(ctx, "/base", nil, &res)
	return res.Changeset, err
}

func (m *MountClient) Status(ctx context.Context) (*StatusResponse, error) {
	var st StatusResponse
	return &st, m.ctl.GetJSON(ctx, "/status", nil, &st)
}

func (m *MountClient) Diff(ctx context.Context) ([]FileStatus, string, error) {
	var out struct {
		Files []FileStatus `json:"files"`
		Patch string       `json:"patch"`
	}
	err := m.ctl.GetJSON(ctx, "/diff", url.Values{}, &out)
	return out.Files, out.Patch, err
}

func (m *MountClient) Commit(ctx context.Context, message, author, land string) (*CommitResult, error) {
	var res CommitResult
	err := m.ctl.PostJSON(ctx, "/commit", map[string]string{"message": message, "author": author, "land": land}, &res)
	return &res, err
}

func (m *MountClient) Update(ctx context.Context, rev string, force bool) (object.ID, error) {
	var res struct {
		Changeset object.ID `json:"changeset"`
	}
	err := m.ctl.PostJSON(ctx, "/update", map[string]any{"rev": rev, "force": force}, &res)
	return res.Changeset, err
}

func (m *MountClient) Land(ctx context.Context, ref string) (*repo.LandResult, error) {
	var res repo.LandResult
	err := m.ctl.PostJSON(ctx, "/land", map[string]string{"ref": ref}, &res)
	return &res, err
}
