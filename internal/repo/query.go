package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	tooLarge := func() error {
		return fmt.Errorf("%w: %s is too large to blame (over %d bytes or %d lines)", ErrInvalid, path, textdiff.MaxBytes, textdiff.MaxLines)
	}
	if bv.TooLarge {
		return nil, tooLarge()
	}
	u, err := r.ReadUnode(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	content, err := r.readText(ctx, u.Content)
	if errors.Is(err, errTooLarge) {
		return nil, tooLarge()
	}
	if err != nil {
		return nil, err
	}
	lines := textdiff.SplitLines(content)
	out := make([]BlameLine, 0, len(lines))
	// Walk the runs rather than expand them into one ID per line.
	for _, run := range bv.Runs {
		for k := 0; k < run[0] && len(out) < len(lines); k++ {
			out = append(out, BlameLine{Line: len(out) + 1, Changeset: bv.Changesets[run[1]], Text: lines[len(out)]})
		}
	}
	if len(out) != len(lines) {
		return nil, fmt.Errorf("blame of %s does not match its content", path)
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

// PatchLimit bounds the content one Patch reads, counting both sides of
// every file, in multiples of the textdiff limits: four times (32 MiB or
// 800,000 lines) keeps a patch of many large files within a couple of
// hundred megabytes of heap and a few seconds of CPU. Files past it are
// named without hunks. A variable so that tests can lower it.
var PatchLimit = 4

// Patch renders a unified diff for the given file diffs. Files too large to
// diff by lines, and the files after the content diffed reaches PatchLimit,
// are named without hunks.
func (r *Repo) Patch(ctx context.Context, diffs []FileDiff) (string, error) {
	var out strings.Builder
	bytesLeft, linesLeft := PatchLimit*textdiff.MaxBytes, PatchLimit*textdiff.MaxLines
	for _, d := range diffs {
		var a, b []byte
		var err error
		aName, bName := "a/"+d.Path, "b/"+d.Path
		if d.Old == nil {
			aName = "/dev/null"
		}
		if d.New == nil {
			bName = "/dev/null"
		}
		fmt.Fprintf(&out, "diff --kudzu a/%s b/%s\n", d.Path, d.Path)
		if d.Old != nil && d.New != nil && d.Old.Mode != d.New.Mode {
			fmt.Fprintf(&out, "old mode %o\nnew mode %o\n", d.Old.Mode, d.New.Mode)
			if d.Old.ID == d.New.ID {
				continue
			}
		}
		if bytesLeft >= 0 && linesLeft >= 0 {
			if d.Old != nil {
				a, err = r.readText(ctx, d.Old.ID)
			}
			if d.New != nil && err == nil {
				b, err = r.readText(ctx, d.New.ID)
			}
			if errors.Is(err, errTooLarge) {
				// Finding that took inflating up to the limit.
				bytesLeft -= textdiff.MaxBytes
				out.WriteString(textdiff.TooLargeNote(aName, bName))
				continue
			}
			if err != nil {
				return "", err
			}
			bytesLeft -= len(a) + len(b)
			linesLeft -= textdiff.CountLines(a) + textdiff.CountLines(b)
		}
		if bytesLeft < 0 || linesLeft < 0 {
			fmt.Fprintf(&out, "Files %s and %s differ (patch too large; diff this path on its own)\n", aName, bName)
			continue
		}
		out.WriteString(textdiff.Unified(aName, bName, a, b, 3))
	}
	return out.String(), nil
}
