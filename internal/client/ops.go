package client

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/textdiff"
)

var ErrNothingToCommit = errors.New("nothing to commit")

type CommitResult struct {
	ID      object.ID        `json:"id"`
	Tree    object.ID        `json:"tree"`
	Changes int              `json:"changes"`
	Land    *repo.LandResult `json:"land,omitempty"`
}

// Commit uploads only the blobs the server lacks and asks the server to
// create the changeset. There is nothing to push afterwards: the changeset
// exists on the server the moment this returns. With land set, the server
// also lands it on that ref in the same call.
func (w *WC) Commit(ctx context.Context, message, author string, addUntracked bool, land string) (*CommitResult, error) {
	st, err := w.Status(ctx)
	if err != nil {
		return nil, err
	}
	var changes []object.Change
	uploads := map[object.ID][]byte{}
	for _, f := range st {
		switch f.Code {
		case 'D':
			changes = append(changes, object.Change{Path: f.Path})
		case '?':
			if !addUntracked {
				continue
			}
			fallthrough
		case 'M':
			content, err := readContent(w.abs(f.Path), f.Mode)
			if err != nil {
				return nil, err
			}
			id, stored := object.EncodeObject(object.KindBlob, content)
			uploads[id] = stored
			if err := w.Objs.Put(ctx, id, stored); err != nil {
				return nil, err
			}
			changes = append(changes, object.Change{Path: f.Path, Mode: f.Mode, Blob: id})
		}
	}
	if len(changes) == 0 {
		return nil, ErrNothingToCommit
	}
	ids := make([]object.ID, 0, len(uploads))
	for id := range uploads {
		ids = append(ids, id)
	}
	missing, err := w.Remote.Missing(ctx, ids)
	if err != nil {
		return nil, err
	}
	toSend := map[object.ID][]byte{}
	for _, id := range missing {
		toSend[id] = uploads[id]
	}
	if err := w.Remote.Upload(ctx, toSend); err != nil {
		return nil, err
	}
	var parents []object.ID
	if !w.State.Changeset.IsZero() {
		parents = []object.ID{w.State.Changeset}
	}
	var resp struct {
		ID   object.ID        `json:"id"`
		Tree object.ID        `json:"tree"`
		Land *repo.LandResult `json:"land"`
	}
	req := map[string]any{"parents": parents, "author": author, "message": message, "changes": changes}
	if land != "" {
		req["land"] = land
	}
	if err := w.Remote.PostJSON(ctx, "/api/changesets", req, &resp); err != nil {
		return nil, err
	}
	for _, ch := range changes {
		if ch.Deleted() {
			delete(w.Index, ch.Path)
			continue
		}
		info, err := os.Lstat(w.abs(ch.Path))
		if err != nil {
			return nil, err
		}
		w.Index[ch.Path] = IndexEntry{Mode: ch.Mode, Blob: ch.Blob, Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
	}
	w.State.Changeset, w.State.Tree = resp.ID, resp.Tree
	if err := w.Save(); err != nil {
		return nil, err
	}
	res := &CommitResult{ID: resp.ID, Tree: resp.Tree, Changes: len(changes), Land: resp.Land}
	if resp.Land != nil && resp.Land.Landed != resp.ID {
		// Landing rebased the change; move the checkout to what landed.
		if err := w.Update(ctx, resp.Land.Landed.String(), false); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (w *WC) dirty(ctx context.Context) (bool, error) {
	st, err := w.Status(ctx)
	if err != nil {
		return false, err
	}
	for _, f := range st {
		if f.Code == 'M' || f.Code == 'D' {
			return true, nil
		}
	}
	return false, nil
}

// Update moves the checkout to rev, touching only files that differ. It
// refuses if tracked files are modified, or if untracked files are where rev
// has files, unless force discards them.
func (w *WC) Update(ctx context.Context, rev string, force bool) error {
	st, err := w.Status(ctx)
	if err != nil {
		return err
	}
	var edited []FileStatus
	for _, f := range st {
		if f.Code == 'M' || f.Code == 'D' {
			edited = append(edited, f)
		}
	}
	if len(edited) > 0 && !force {
		return errors.New("working copy has uncommitted changes (commit them or use --force)")
	}
	res, err := w.resolve(ctx, rev)
	if err != nil {
		return err
	}
	writes := map[string]object.Entry{}
	var deletes []string
	if res.Tree != w.State.Tree {
		if writes, deletes, err = w.diffTrees(ctx, w.State.Tree, res.Tree, w.State.Sparse); err != nil {
			return err
		}
	}
	// --force: also restore edited files the two revisions have in common,
	// which the tree diff alone would leave untouched.
	removed := map[string]bool{}
	for _, p := range deletes {
		removed[p] = true
	}
	for _, f := range edited {
		ie, ok := w.Index[f.Path]
		if _, written := writes[f.Path]; ok && !written && !removed[f.Path] {
			writes[f.Path] = object.Entry{Mode: ie.Mode, ID: ie.Blob}
			ie.MtimeNs = -1 // force a rewrite
			w.Index[f.Path] = ie
		}
	}
	if len(writes) > 0 || len(deletes) > 0 {
		fmt.Fprintf(w.Out, "updating %d %s, removing %d\n", len(writes), pick(len(writes) == 1, "file", "files"), len(deletes))
		if err := w.apply(ctx, writes, deletes, force); err != nil {
			return err
		}
	}
	w.State.Changeset, w.State.Tree = res.Changeset, res.Tree
	return w.Save()
}

// SetSparse changes which directories are checked out.
func (w *WC) SetSparse(ctx context.Context, sp sparse.Spec) error {
	if d, err := w.dirty(ctx); err != nil {
		return err
	} else if d {
		return errors.New("working copy has uncommitted changes")
	}
	files, err := w.collectFiles(ctx, w.State.Tree, sp)
	if err != nil {
		return err
	}
	var deletes []string
	for p := range w.Index {
		if _, ok := files[p]; !ok {
			deletes = append(deletes, p)
		}
	}
	if err := w.apply(ctx, files, deletes, false); err != nil {
		return err
	}
	w.State.Sparse = sp
	return w.Save()
}

// Land asks the server to land the checked-out changeset (and the stack below
// it) on ref, then moves the checkout to the landed changeset.
func (w *WC) Land(ctx context.Context, ref string) (*repo.LandResult, error) {
	var res repo.LandResult
	err := w.Remote.PostJSON(ctx, "/api/land", map[string]string{"head": w.State.Changeset.String(), "ref": ref}, &res)
	if err != nil {
		return nil, err
	}
	if res.Landed != w.State.Changeset {
		if err := w.Update(ctx, res.Landed.String(), false); err != nil {
			return nil, err
		}
	}
	return &res, nil
}

// Rebase moves the checked-out stack onto rev on the server.
func (w *WC) Rebase(ctx context.Context, onto string) (object.ID, error) {
	var res struct {
		Head object.ID `json:"head"`
	}
	err := w.Remote.PostJSON(ctx, "/api/rebase", map[string]string{"head": w.State.Changeset.String(), "onto": onto}, &res)
	if err != nil {
		return object.Zero, err
	}
	return res.Head, w.Update(ctx, res.Head.String(), false)
}

// LocalDiff renders the uncommitted changes as a unified diff.
func (w *WC) LocalDiff(ctx context.Context) (string, error) {
	st, err := w.Status(ctx)
	if err != nil {
		return "", err
	}
	var out string
	for _, f := range st {
		if f.Code == '?' {
			continue
		}
		var old, cur []byte
		if ie, ok := w.Index[f.Path]; ok {
			if old, err = w.readBlob(ctx, ie.Blob); err != nil {
				return "", err
			}
		}
		bName := "b/" + f.Path
		if f.Code == 'D' {
			bName = "/dev/null"
		} else if cur, err = readContent(w.abs(f.Path), f.Mode); err != nil {
			return "", err
		}
		out += "diff --kudzu a/" + f.Path + " b/" + f.Path + "\n"
		out += textdiff.Unified("a/"+f.Path, bName, old, cur, 3)
	}
	return out, nil
}
