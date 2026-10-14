package repo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/textdiff"
)

// Log returns up to limit changesets along head's first-parent line.
func (r *Repo) Log(ctx context.Context, head object.ID, limit int) ([]object.ID, error) {
	var out []object.ID
	for id := head; !id.IsZero() && len(out) < limit; {
		out = append(out, id)
		c, err := r.Changeset(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(c.Parents) == 0 {
			break
		}
		id = c.Parents[0]
	}
	return out, nil
}

// PathLog returns the changesets that changed path (a file or a directory),
// newest first. It follows the path's unode chain, so it reads one node per
// result no matter how much unrelated history lies in between.
func (r *Repo) PathLog(ctx context.Context, head object.ID, path string, limit int) ([]object.ID, error) {
	root, err := r.UnodeOf(ctx, head)
	if err != nil {
		return nil, err
	}
	e, ok, err := lookup(ctx, unodeOps{r: r}, root, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s: %w at %s", path, store.ErrNotFound, head.Short())
	}
	var out []object.ID
	for id := e.ID; !id.IsZero() && len(out) < limit; {
		u, err := r.ReadUnode(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, u.Linknode)
		if len(u.Parents) == 0 {
			break
		}
		id = u.Parents[0]
	}
	return out, nil
}

type BlameLine struct {
	Line      int       `json:"line"`
	Changeset object.ID `json:"changeset"`
	Text      string    `json:"text"`
}

func (r *Repo) Blame(ctx context.Context, head object.ID, path string) ([]BlameLine, error) {
	root, err := r.UnodeOf(ctx, head)
	if err != nil {
		return nil, err
	}
	e, ok, err := lookup(ctx, unodeOps{r: r}, root, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s: %w at %s", path, store.ErrNotFound, head.Short())
	}
	if e.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrInvalid, path)
	}
	v, err := r.Derive(ctx, BlameDeriver, e.ID)
	if err != nil {
		return nil, err
	}
	var bv blameValue
	if err := json.Unmarshal(v, &bv); err != nil {
		return nil, err
	}
	if bv.Binary {
		return nil, fmt.Errorf("%w: %s is not a text file", ErrInvalid, path)
	}
	u, err := r.ReadUnode(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	content, err := r.ReadBlob(ctx, u.Content)
	if err != nil {
		return nil, err
	}
	attr := bv.expand()
	lines := textdiff.SplitLines(content)
	out := make([]BlameLine, len(lines))
	for i, l := range lines {
		out[i] = BlameLine{Line: i + 1, Changeset: attr[i], Text: l}
	}
	return out, nil
}

// ReadFile returns the entry and content of a file at a changeset.
func (r *Repo) ReadFile(ctx context.Context, cs object.ID, path string) (object.Entry, []byte, error) {
	tree, err := r.TreeOf(ctx, cs)
	if err != nil {
		return object.Entry{}, nil, err
	}
	e, ok, err := r.LookupPath(ctx, tree, path)
	if err != nil {
		return object.Entry{}, nil, err
	}
	if !ok {
		return object.Entry{}, nil, fmt.Errorf("%s: %w at %s", path, store.ErrNotFound, cs.Short())
	}
	if e.IsDir() {
		return e, nil, fmt.Errorf("%w: %s is a directory", ErrInvalid, path)
	}
	b, err := r.ReadBlob(ctx, e.ID)
	return e, b, err
}

// ListDir returns the entries of a directory at a changeset.
func (r *Repo) ListDir(ctx context.Context, cs object.ID, path string) ([]object.Entry, error) {
	tree, err := r.TreeOf(ctx, cs)
	if err != nil {
		return nil, err
	}
	e, ok, err := r.LookupPath(ctx, tree, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s: %w at %s", path, store.ErrNotFound, cs.Short())
	}
	if !e.IsDir() {
		return []object.Entry{e}, nil
	}
	t, err := r.ReadTree(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	return t.Entries, nil
}

// Diff returns the files that differ between two changesets, optionally
// restricted to a path prefix.
func (r *Repo) Diff(ctx context.Context, from, to object.ID, path string) ([]FileDiff, error) {
	var a, b object.ID
	var err error
	if !from.IsZero() {
		if a, err = r.TreeOf(ctx, from); err != nil {
			return nil, err
		}
	}
	if b, err = r.TreeOf(ctx, to); err != nil {
		return nil, err
	}
	if path != "" {
		ea, _, err := r.LookupPath(ctx, a, path)
		if err != nil {
			return nil, err
		}
		eb, _, err := r.LookupPath(ctx, b, path)
		if err != nil {
			return nil, err
		}
		if !ea.IsDir() || !eb.IsDir() {
			// A single file (or a file/dir swap): compare the entries directly.
			var out []FileDiff
			fd := FileDiff{Path: path}
			if !ea.ID.IsZero() && !ea.IsDir() {
				fd.Old = &ea
			}
			if !eb.ID.IsZero() && !eb.IsDir() {
				fd.New = &eb
			}
			if (fd.Old != nil || fd.New != nil) && (fd.Old == nil || fd.New == nil || *fd.Old != *fd.New) {
				out = append(out, fd)
			}
			return out, nil
		}
		a, b = ea.ID, eb.ID
	}
	var out []FileDiff
	err = r.DiffTrees(ctx, a, b, path, func(d FileDiff) error {
		out = append(out, d)
		return nil
	})
	return out, err
}

// Patch renders a unified diff for the given file diffs.
func (r *Repo) Patch(ctx context.Context, diffs []FileDiff) (string, error) {
	var out []byte
	for _, d := range diffs {
		var a, b []byte
		var err error
		aName, bName := "a/"+d.Path, "b/"+d.Path
		if d.Old != nil {
			if a, err = r.ReadBlob(ctx, d.Old.ID); err != nil {
				return "", err
			}
		} else {
			aName = "/dev/null"
		}
		if d.New != nil {
			if b, err = r.ReadBlob(ctx, d.New.ID); err != nil {
				return "", err
			}
		} else {
			bName = "/dev/null"
		}
		header := fmt.Sprintf("diff --kudzu a/%s b/%s\n", d.Path, d.Path)
		if d.Old != nil && d.New != nil && d.Old.Mode != d.New.Mode {
			header += fmt.Sprintf("old mode %o\nnew mode %o\n", d.Old.Mode, d.New.Mode)
		}
		out = append(out, header...)
		out = append(out, textdiff.Unified(aName, bName, a, b, 3)...)
	}
	return string(out), nil
}
