package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/textdiff"
)

// Overlay is a writable view of a revision, kept the way overlayfs keeps
// one: the revision (the base, an FS) stays lazily fetched and untouched,
// files that are written or created live in full under dir/upper, and base
// paths that are deleted are recorded as whiteouts. Nothing of the base is
// copied until a file is written, so editing one file in a huge revision
// fetches one blob. Commit turns the overlay into a changeset on the server
// and starts over on top of it.
//
// Paths are slash-separated and relative to the root ("" is the root).
type Overlay struct {
	dir    string
	remote *proto.Remote
	cache  store.Objects
	sparse sparse.Spec

	mu        sync.RWMutex
	base      *FS
	whiteouts map[string]bool
	writers   map[*os.File]string // files open for writing, and their paths
}

type overlayState struct {
	Changeset object.ID   `json:"changeset"`
	Sparse    sparse.Spec `json:"sparse,omitempty"`
	Whiteouts []string    `json:"whiteouts,omitempty"`
}

// Overlay errors, so that a filesystem can map them to errno values.
var (
	ErrNotDir   = errNotDir
	ErrIsDir    = errors.New("is a directory")
	ErrExist    = fs.ErrExist
	ErrNotEmpty = errors.New("directory not empty")
	ErrCrossDir = errors.New("cannot rename a directory that has files in the base revision")
	ErrInvalid  = errors.New("invalid name")
)

// OpenOverlay opens (or creates) the overlay kept in dir. A new overlay
// starts at rev; an existing one resumes at its own base revision, and rev
// must then be empty or resolve to that revision unless the overlay has no
// changes.
func OpenOverlay(ctx context.Context, dir string, remote *proto.Remote, rev string, sp sparse.Spec, cache store.Objects) (*Overlay, error) {
	if err := os.MkdirAll(filepath.Join(dir, "upper"), 0o700); err != nil {
		return nil, err
	}
	if cache == nil {
		cache = store.NewMem(256 << 20)
	}
	o := &Overlay{dir: dir, remote: remote, cache: cache, sparse: sp, whiteouts: map[string]bool{}, writers: map[*os.File]string{}}
	var st overlayState
	b, err := os.ReadFile(o.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("corrupt overlay state %s: %w", o.statePath(), err)
		}
		for _, w := range st.Whiteouts {
			o.whiteouts[w] = true
		}
		o.sparse = st.Sparse
		if o.base, err = Open(ctx, remote, st.Changeset.String(), o.sparse, cache); err != nil {
			return nil, err
		}
		if rev != "" {
			var res struct {
				Changeset object.ID `json:"changeset"`
			}
			if err := remote.GetJSON(ctx, "/api/resolve", url.Values{"rev": {rev}}, &res); err != nil {
				return nil, err
			}
			if res.Changeset != st.Changeset || fmt.Sprint(sp) != fmt.Sprint(o.sparse) {
				changes, err := o.changes(ctx)
				if err != nil {
					return nil, err
				}
				if len(changes) > 0 {
					return nil, fmt.Errorf("the overlay in %s has %d uncommitted changes on top of %s; mount without -r and --sparse to resume, or commit them first", dir, len(changes), st.Changeset.Short())
				}
				o.sparse = sp
				if err := o.reset(ctx, res.Changeset.String()); err != nil {
					return nil, err
				}
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		if rev == "" {
			rev = "main"
		}
		if o.base, err = Open(ctx, remote, rev, sp, cache); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return o, o.save()
}

func (o *Overlay) statePath() string { return filepath.Join(o.dir, "overlay.json") }

func (o *Overlay) upper(p string) string {
	return filepath.Join(o.dir, "upper", filepath.FromSlash(p))
}

// save writes the overlay state; callers hold o.mu (or are constructing o).
func (o *Overlay) save() error {
	st := overlayState{Changeset: o.base.Changeset, Sparse: o.sparse}
	for w := range o.whiteouts {
		st.Whiteouts = append(st.Whiteouts, w)
	}
	sort.Strings(st.Whiteouts)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.statePath())
}

// Base is the revision the overlay sits on.
func (o *Overlay) Base() *FS {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.base
}

// Entry is a path in the merged view.
type Entry struct {
	Path  string
	Mode  uint32 // object.ModeDir, ModeFile, ModeExec or ModeSymlink
	Size  int64
	Upper bool // in the overlay rather than the base
	node  Node // the base node, when !Upper
}

func (e Entry) IsDir() bool { return e.Mode == object.ModeDir }

func under(p, dir string) bool { return dir == "" || p == dir || strings.HasPrefix(p, dir+"/") }

// hidden reports whether a whiteout covers p or one of its ancestors.
// Callers hold o.mu.
func (o *Overlay) hidden(p string) bool {
	for {
		if o.whiteouts[p] {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

func modeOf(info os.FileInfo) uint32 {
	switch {
	case info.IsDir():
		return object.ModeDir
	case info.Mode()&fs.ModeSymlink != 0:
		return object.ModeSymlink
	case info.Mode().Perm()&0o111 != 0:
		return object.ModeExec
	}
	return object.ModeFile
}

// upperEntry returns p from the overlay, if it is there.
func (o *Overlay) upperEntry(p string) (Entry, bool) {
	info, err := os.Lstat(o.upper(p))
	if err != nil {
		return Entry{}, false
	}
	e := Entry{Path: p, Mode: modeOf(info), Upper: true}
	if !info.IsDir() {
		e.Size = info.Size()
	}
	return e, true
}

// stat finds p in the merged view. Callers hold o.mu.
func (o *Overlay) stat(ctx context.Context, p string) (Entry, error) {
	if p == "" {
		return Entry{Mode: object.ModeDir, node: o.base.Root()}, nil
	}
	// An ancestor that the overlay replaced with a file hides everything
	// below it, whatever the base has there.
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if e, ok := o.upperEntry(p[:i]); ok && !e.IsDir() {
				return Entry{}, fs.ErrNotExist
			}
		}
	}
	if e, ok := o.upperEntry(p); ok {
		return e, nil
	}
	if o.hidden(p) {
		return Entry{}, fs.ErrNotExist
	}
	n, err := o.base.Stat(ctx, p)
	if errors.Is(err, errNotDir) {
		return Entry{}, fs.ErrNotExist
	}
	if err != nil {
		return Entry{}, err
	}
	return Entry{Path: p, Mode: n.Mode, Size: n.Size, node: n}, nil
}

// Stat finds p in the merged view.
func (o *Overlay) Stat(ctx context.Context, p string) (Entry, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.stat(ctx, p)
}

// inBase reports whether the base revision has p, ignoring the overlay.
func (o *Overlay) inBase(ctx context.Context, p string) (Node, bool, error) {
	n, err := o.base.Stat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotDir) {
		return Node{}, false, nil
	}
	return n, err == nil, err
}

// ReadDir lists a directory of the merged view, sorted by name.
func (o *Overlay) ReadDir(ctx context.Context, p string) ([]Entry, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.readDir(ctx, p)
}

func (o *Overlay) readDir(ctx context.Context, p string) ([]Entry, error) {
	dir, err := o.stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, errNotDir
	}
	byName := map[string]Entry{}
	// Base entries, unless the overlay replaced this directory wholesale.
	if !o.hidden(p) {
		if n, ok, err := o.inBase(ctx, p); err != nil {
			return nil, err
		} else if ok && n.IsDir() {
			nodes, err := o.base.ReadDir(ctx, n)
			if err != nil {
				return nil, err
			}
			for _, c := range nodes {
				if !o.hidden(c.Path) {
					byName[path.Base(c.Path)] = Entry{Path: c.Path, Mode: c.Mode, Size: c.Size, node: c}
				}
			}
		}
	}
	if ents, err := os.ReadDir(o.upper(p)); err == nil {
		for _, de := range ents {
			cp := object.JoinPath(p, de.Name())
			if e, ok := o.upperEntry(cp); ok {
				byName[de.Name()] = e
			}
		}
	}
	out := make([]Entry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ReadFile returns a file's content or a symlink's target.
func (o *Overlay) ReadFile(ctx context.Context, p string) ([]byte, error) {
	o.mu.RLock()
	e, err := o.stat(ctx, p)
	base := o.base
	o.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	switch {
	case e.IsDir():
		return nil, ErrIsDir
	case e.Upper && e.Mode == object.ModeSymlink:
		t, err := os.Readlink(o.upper(p))
		return []byte(t), err
	case e.Upper:
		return os.ReadFile(o.upper(p))
	}
	return base.ReadFile(ctx, e.node)
}

// checkName refuses names a changeset could not hold.
func checkName(p string) error {
	if err := object.ValidatePath(p); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// parentDir makes sure p's parent is a directory in the merged view and
// exists in the overlay. Callers hold o.mu for writing.
func (o *Overlay) parentDir(ctx context.Context, p string) error {
	parent := ""
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		parent = p[:i]
	}
	e, err := o.stat(ctx, parent)
	if err != nil {
		return err
	}
	if !e.IsDir() {
		return errNotDir
	}
	return os.MkdirAll(o.upper(parent), 0o755)
}

// copyUp makes sure the overlay holds p, copying it from the base if
// needed. Callers hold o.mu for writing.
func (o *Overlay) copyUp(ctx context.Context, p string) (Entry, error) {
	e, err := o.stat(ctx, p)
	if err != nil || e.Upper {
		return e, err
	}
	if err := o.parentDir(ctx, p); err != nil {
		return Entry{}, err
	}
	switch e.Mode {
	case object.ModeDir:
		err = os.Mkdir(o.upper(p), 0o755)
	case object.ModeSymlink:
		var target []byte
		if target, err = o.base.ReadFile(ctx, e.node); err == nil {
			err = os.Symlink(string(target), o.upper(p))
		}
	default:
		var data []byte
		if data, err = o.base.ReadFile(ctx, e.node); err == nil {
			perm := os.FileMode(0o644)
			if e.Mode == object.ModeExec {
				perm = 0o755
			}
			err = writeNew(o.upper(p), data, perm)
		}
	}
	if err != nil {
		return Entry{}, err
	}
	e.Upper, e.node = true, Node{}
	return e, nil
}

// writeNew writes a file through a temporary name, so a crash never leaves
// a half-copied file in the overlay.
func writeNew(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".kudzu-tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// OpenWrite opens p for writing, creating it (mode 0644, or 0755 with
// exec) if it does not exist, and copying it up from the base if it does
// (unless trunc, which empties it anyway). Close the file with CloseWrite.
func (o *Overlay) OpenWrite(ctx context.Context, p string, create, excl, trunc, exec bool) (*os.File, error) {
	if err := checkName(p); err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.stat(ctx, p)
	switch {
	case err == nil && excl:
		return nil, ErrExist
	case err == nil && e.IsDir():
		return nil, ErrIsDir
	case errors.Is(err, fs.ErrNotExist) && create:
		if err := o.parentDir(ctx, p); err != nil {
			return nil, err
		}
		perm := os.FileMode(0o644)
		if exec {
			perm = 0o755
		}
		f, err := os.OpenFile(o.upper(p), os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
		if err == nil {
			o.writers[f] = p
		}
		return f, err
	case err != nil:
		return nil, err
	}
	if !e.Upper {
		if trunc && e.Mode != object.ModeSymlink {
			if err := o.parentDir(ctx, p); err != nil {
				return nil, err
			}
			perm := os.FileMode(0o644)
			if e.Mode == object.ModeExec {
				perm = 0o755
			}
			if err := writeNew(o.upper(p), nil, perm); err != nil {
				return nil, err
			}
		} else if _, err := o.copyUp(ctx, p); err != nil {
			return nil, err
		}
	}
	flags := os.O_RDWR
	if trunc {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(o.upper(p), flags, 0)
	if err == nil {
		o.writers[f] = p
	}
	return f, err
}

// CloseWrite closes a file opened by OpenWrite.
func (o *Overlay) CloseWrite(f *os.File) error {
	err := f.Close()
	o.mu.Lock()
	delete(o.writers, f)
	o.mu.Unlock()
	return err
}

// FileReader is a file open for reading.
type FileReader interface {
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	Close() error
}

type upperReader struct{ f *os.File }

func (u upperReader) ReadAt(_ context.Context, p []byte, off int64) (int, error) {
	return u.f.ReadAt(p, off)
}
func (u upperReader) Close() error { return u.f.Close() }

type baseReader struct{ h *Handle }

func (b baseReader) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	return b.h.ReadAt(ctx, p, off)
}
func (b baseReader) Close() error { b.h.Close(); return nil }

// OpenRead opens a file for reading: from the overlay if it is there,
// otherwise from the base, fetched on the first read.
func (o *Overlay) OpenRead(ctx context.Context, p string) (FileReader, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	e, err := o.stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if e.IsDir() {
		return nil, ErrIsDir
	}
	if e.Upper {
		f, err := os.Open(o.upper(p))
		if err != nil {
			return nil, err
		}
		return upperReader{f}, nil
	}
	return baseReader{o.base.OpenFile(e.node)}, nil
}

// SetMode sets or clears a file's executable bit.
func (o *Overlay) SetMode(ctx context.Context, p string, exec bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.stat(ctx, p)
	if err != nil {
		return err
	}
	if e.Mode == object.ModeDir || e.Mode == object.ModeSymlink || (e.Mode == object.ModeExec) == exec {
		return nil
	}
	if _, err := o.copyUp(ctx, p); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if exec {
		perm = 0o755
	}
	return os.Chmod(o.upper(p), perm)
}

// Truncate sets a file's size.
func (o *Overlay) Truncate(ctx context.Context, p string, size int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.stat(ctx, p)
	if err != nil {
		return err
	}
	if e.IsDir() {
		return ErrIsDir
	}
	if !e.Upper && size == 0 {
		perm := os.FileMode(0o644)
		if e.Mode == object.ModeExec {
			perm = 0o755
		}
		if err := o.parentDir(ctx, p); err != nil {
			return err
		}
		return writeNew(o.upper(p), nil, perm)
	}
	if _, err := o.copyUp(ctx, p); err != nil {
		return err
	}
	return os.Truncate(o.upper(p), size)
}

// Mkdir creates a directory.
func (o *Overlay) Mkdir(ctx context.Context, p string) error {
	if err := checkName(p); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.stat(ctx, p); err == nil {
		return ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := o.parentDir(ctx, p); err != nil {
		return err
	}
	// A whiteout on p keeps hiding what the base had there: the new
	// directory starts empty.
	return os.Mkdir(o.upper(p), 0o755)
}

// Symlink creates a symbolic link at p.
func (o *Overlay) Symlink(ctx context.Context, target, p string) error {
	if err := checkName(p); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, err := o.stat(ctx, p); err == nil {
		return ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := o.parentDir(ctx, p); err != nil {
		return err
	}
	return os.Symlink(target, o.upper(p))
}

// whiteout records that the base's p is deleted (if the base has it), and
// drops whiteouts below p, which it now covers. Callers hold o.mu.
func (o *Overlay) whiteout(ctx context.Context, p string) error {
	if _, ok, err := o.inBase(ctx, p); err != nil || !ok {
		return err
	}
	for w := range o.whiteouts {
		if under(w, p) {
			delete(o.whiteouts, w)
		}
	}
	o.whiteouts[p] = true
	return o.save()
}

// Remove deletes a file or symlink.
func (o *Overlay) Remove(ctx context.Context, p string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.stat(ctx, p)
	if err != nil {
		return err
	}
	if e.IsDir() {
		return ErrIsDir
	}
	if e.Upper {
		if err := os.Remove(o.upper(p)); err != nil {
			return err
		}
	}
	return o.whiteout(ctx, p)
}

// Rmdir deletes an empty directory.
func (o *Overlay) Rmdir(ctx context.Context, p string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.stat(ctx, p)
	if err != nil {
		return err
	}
	if !e.IsDir() {
		return errNotDir
	}
	if p == "" {
		return ErrInvalid
	}
	children, err := o.readDir(ctx, p)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return ErrNotEmpty
	}
	if e.Upper {
		if err := os.RemoveAll(o.upper(p)); err != nil {
			return err
		}
	}
	return o.whiteout(ctx, p)
}

// Rename moves a file, symlink or overlay-only directory. A directory with
// files in the base would have to be copied whole, so it is refused with
// ErrCrossDir, which a filesystem reports as EXDEV: tools like mv then copy
// and delete instead.
func (o *Overlay) Rename(ctx context.Context, from, to string) error {
	if err := checkName(to); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	e, err := o.stat(ctx, from)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if under(to, from) {
		return ErrInvalid
	}
	if dst, err := o.stat(ctx, to); err == nil {
		switch {
		case dst.IsDir() && !e.IsDir():
			return ErrIsDir
		case !dst.IsDir() && e.IsDir():
			return errNotDir
		case dst.IsDir():
			if children, err := o.readDir(ctx, to); err != nil {
				return err
			} else if len(children) > 0 {
				return ErrNotEmpty
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if e.IsDir() {
		if _, ok, err := o.inBase(ctx, from); err != nil {
			return err
		} else if ok && !o.hidden(from) {
			return ErrCrossDir
		}
	} else if _, err := o.copyUp(ctx, from); err != nil {
		return err
	}
	if err := o.parentDir(ctx, to); err != nil {
		return err
	}
	if err := os.Rename(o.upper(from), o.upper(to)); err != nil {
		return err
	}
	for f, p := range o.writers {
		if under(p, from) {
			o.writers[f] = to + p[len(from):]
		}
	}
	return o.whiteout(ctx, from)
}

// FileStatus is one changed path: A added, M modified, D deleted.
type FileStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	change object.Change
	data   []byte
}

// Changes lists what the overlay changes relative to its base, sorted by
// path. It reads every file in the overlay (they are local) and lists the
// base trees below whiteouts, but fetches no file from the base.
func (o *Overlay) Changes(ctx context.Context) ([]FileStatus, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.changes(ctx)
}

func (o *Overlay) changes(ctx context.Context) ([]FileStatus, error) {
	var out []FileStatus
	inUpper := map[string]bool{}
	root := filepath.Join(o.dir, "upper")
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, full)
		p := filepath.ToSlash(rel)
		if strings.HasSuffix(p, ".kudzu-tmp") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := modeOf(info)
		var data []byte
		if mode == object.ModeSymlink {
			t, err := os.Readlink(full)
			if err != nil {
				return err
			}
			data = []byte(t)
		} else if !info.Mode().IsRegular() {
			return nil
		} else if data, err = os.ReadFile(full); err != nil {
			return err
		}
		inUpper[p] = true
		blob := object.Hash(object.KindBlob, data)
		status := "A"
		if n, ok, err := o.inBase(ctx, p); err != nil {
			return err
		} else if ok && !n.IsDir() {
			if n.Mode == mode && n.ID == blob {
				return nil // written back unchanged
			}
			status = "M"
		}
		out = append(out, FileStatus{Path: p, Status: status, change: object.Change{Path: p, Mode: mode, Blob: blob}, data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for w := range o.whiteouts {
		n, ok, err := o.inBase(ctx, w)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		err = o.base.walk(ctx, n, func(f Node) error {
			if !inUpper[f.Path] {
				out = append(out, FileStatus{Path: f.Path, Status: "D", change: object.Change{Path: f.Path}})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// walk calls fn for every file at or below n.
func (f *FS) walk(ctx context.Context, n Node, fn func(Node) error) error {
	if !n.IsDir() {
		return fn(n)
	}
	children, err := f.ReadDir(ctx, n)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := f.walk(ctx, c, fn); err != nil {
			return err
		}
	}
	return nil
}

// Diff renders the overlay's changes as a unified diff.
func (o *Overlay) Diff(ctx context.Context) ([]FileStatus, string, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	changes, err := o.changes(ctx)
	if err != nil {
		return nil, "", err
	}
	var b strings.Builder
	for _, c := range changes {
		var old []byte
		if c.Status != "A" {
			n, _, err := o.inBase(ctx, c.Path)
			if err != nil {
				return nil, "", err
			}
			if old, err = o.base.ReadFile(ctx, n); err != nil {
				return nil, "", err
			}
		}
		aName, bName := "a/"+c.Path, "b/"+c.Path
		if c.Status == "A" {
			aName = "/dev/null"
		}
		if c.Status == "D" {
			bName = "/dev/null"
		}
		b.WriteString("diff --kudzu a/" + c.Path + " b/" + c.Path + "\n")
		b.WriteString(textdiff.Unified(aName, bName, old, c.data, 3))
	}
	return changes, b.String(), nil
}

// ErrNothingToCommit means the overlay has no changes.
var ErrNothingToCommit = errors.New("nothing to commit")

// CommitResult describes a commit made from the overlay.
type CommitResult struct {
	ID      object.ID        `json:"id"`
	Changes int              `json:"changes"`
	Land    *repo.LandResult `json:"land,omitempty"`
	Base    object.ID        `json:"base"` // the overlay's new base
}

// Commit creates a changeset from the overlay's changes on the server
// (uploading only the blobs it lacks), optionally lands it on a ref, and
// then makes the result the new base and empties the overlay. It refuses
// while files are open for writing, since what they will still write would
// be lost.
func (o *Overlay) Commit(ctx context.Context, message, author, land string) (*CommitResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkNoWriters(); err != nil {
		return nil, err
	}
	changes, err := o.changes(ctx)
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, ErrNothingToCommit
	}
	uploads := map[object.ID][]byte{}
	req := map[string]any{"parents": []object.ID{o.base.Changeset}, "author": author, "message": message}
	var list []object.Change
	for _, c := range changes {
		list = append(list, c.change)
		if c.Status != "D" {
			uploads[c.change.Blob] = object.Encode(object.KindBlob, c.data)
		}
	}
	req["changes"] = list
	if land != "" {
		req["land"] = land
	}
	ids := make([]object.ID, 0, len(uploads))
	for id := range uploads {
		ids = append(ids, id)
	}
	missing, err := o.remote.Missing(ctx, ids)
	if err != nil {
		return nil, err
	}
	send := map[object.ID][]byte{}
	for _, id := range missing {
		send[id] = uploads[id]
	}
	if err := o.remote.Upload(ctx, send); err != nil {
		return nil, err
	}
	var resp struct {
		ID   object.ID        `json:"id"`
		Land *repo.LandResult `json:"land"`
	}
	if err := o.remote.PostJSON(ctx, "/api/changesets", req, &resp); err != nil {
		return nil, err
	}
	res := &CommitResult{ID: resp.ID, Changes: len(changes), Land: resp.Land, Base: resp.ID}
	if resp.Land != nil {
		res.Base = resp.Land.Landed
	}
	return res, o.reset(ctx, res.Base.String())
}

func (o *Overlay) checkNoWriters() error {
	if len(o.writers) == 0 {
		return nil
	}
	var open []string
	for _, p := range o.writers {
		open = append(open, p)
	}
	sort.Strings(open)
	return fmt.Errorf("files are open for writing: %s; close them first", strings.Join(open, ", "))
}

// reset moves the overlay to rev and empties it. Callers hold o.mu.
func (o *Overlay) reset(ctx context.Context, rev string) error {
	base, err := Open(ctx, o.remote, rev, o.sparse, o.cache)
	if err != nil {
		return err
	}
	upper := filepath.Join(o.dir, "upper")
	if err := os.RemoveAll(upper); err != nil {
		return err
	}
	if err := os.Mkdir(upper, 0o700); err != nil {
		return err
	}
	o.base, o.whiteouts = base, map[string]bool{}
	return o.save()
}

// Update moves the overlay to another revision. It refuses if the overlay
// has changes, unless force, which discards them.
func (o *Overlay) Update(ctx context.Context, rev string, force bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkNoWriters(); err != nil {
		return err
	}
	if !force {
		changes, err := o.changes(ctx)
		if err != nil {
			return err
		}
		if len(changes) > 0 {
			return fmt.Errorf("the mount has %d uncommitted changes (commit them or use --force)", len(changes))
		}
	}
	return o.reset(ctx, rev)
}
