package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
)

func (k *kudzu) addWriteTools(s *sdk.Server) {
	addTool(s, &sdk.Tool{
		Name: "kudzu_write",
		Description: "Create a changeset from file contents without a checkout: write text files and/or delete paths on top of a parent revision, and optionally land it on a ref in the same call. " +
			"Set parent to the changeset ID you read the files at (kudzu_cat and kudzu_resolve report it), not a moving ref, so that changes landed since then are merged instead of silently overwritten. " +
			"Landing rebases onto the ref's current tip on the server; if someone else changed the same or adjacent lines, the call fails with a conflict that lists the paths, and the changeset is kept but not landed.",
		Annotations: additive("Write files", false),
	}, k.writeTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_land",
		Description: "Land an existing changeset (and the unlanded changesets below it) onto a ref without any lock: the server rebases it onto the ref's current tip and moves the ref with compare-and-swap, retrying if another land wins the race. Landing something already on the ref is a no-op. Fails with a conflict listing the paths when the rebase cannot merge.",
		Annotations: additive("Land changeset", true),
	}, k.landTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_rebase",
		Description: "Rebase a changeset (and the changesets between it and the merge base) onto another revision on the server, creating new changesets; no ref moves. Returns the new head. Fails with a conflict listing the paths when a change cannot be merged.",
		Annotations: additive("Rebase", true),
	}, k.rebaseTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_wc_commit",
		Description: "Commit the local changes of a Kudzu working copy on this machine as a new changeset (uploading only file contents the server lacks), optionally landing it on a ref in the same call. On a conflict nothing changes locally; see the error for how to resolve it.",
		Annotations: additive("Commit working copy", false),
	}, k.wcCommitTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_wc_update",
		Description: "Move a Kudzu working copy on this machine to another revision (default main), rewriting only files that differ. Refuses when the working copy has uncommitted changes to tracked files; untracked files at paths the new revision adds are overwritten.",
		Annotations: destructive("Update working copy"),
	}, k.wcUpdateTool)
}

type writeFile struct {
	Path       string `json:"path" jsonschema:"Repository-relative path, e.g. services/api/handler.go."`
	Content    string `json:"content" jsonschema:"The complete new content of the file (UTF-8 text)."`
	Executable *bool  `json:"executable,omitempty" jsonschema:"Set the executable bit. Default: keep the file's current mode (new files are not executable)."`
}

type writeIn struct {
	Parent  string      `json:"parent" jsonschema:"Revision the new contents are based on. Prefer the exact changeset ID you read the files at; 'main' means main as it is right now."`
	Message string      `json:"message" jsonschema:"Changeset message: a one-line summary, optionally followed by a blank line and details."`
	Files   []writeFile `json:"files,omitempty" jsonschema:"Files to create or overwrite."`
	Delete  []string    `json:"delete,omitempty" jsonschema:"Paths of files to delete."`
	Land    string      `json:"land,omitempty" jsonschema:"Ref to land the new changeset on, e.g. main. Default: do not land."`
	Author  string      `json:"author,omitempty" jsonschema:"Author as 'Name <email>'. Default: the author kudzu mcp was started with (--author, $KUDZU_AUTHOR or user@host)."`
}

type landOut struct {
	Ref      string    `json:"ref"`
	Landed   object.ID `json:"landed"`
	Previous object.ID `json:"previous"`
	Rebased  bool      `json:"rebased"`
	Attempts int       `json:"attempts"`
	Noop     bool      `json:"already_landed,omitempty"`
}

func landView(ref string, r *repo.LandResult) *landOut {
	return &landOut{Ref: ref, Landed: r.Landed, Previous: r.Previous, Rebased: r.Rebased, Attempts: r.Attempts, Noop: r.Landed == r.Previous}
}

func (k *kudzu) land(ctx context.Context, head object.ID, ref string) (*repo.LandResult, error) {
	var res repo.LandResult
	err := k.rem.PostJSON(ctx, "/api/land", map[string]string{"head": head.String(), "ref": ref}, &res)
	return &res, err
}

func (k *kudzu) writeTool(ctx context.Context, in writeIn) (*sdk.CallToolResult, error) {
	if len(in.Files) == 0 && len(in.Delete) == 0 {
		return nil, errors.New("nothing to write: pass files and/or delete")
	}
	if strings.TrimSpace(in.Message) == "" {
		return nil, errors.New("message must not be empty")
	}
	parent, err := k.resolve(ctx, in.Parent)
	if err != nil {
		return nil, fmt.Errorf("parent: %w", err)
	}
	modes, err := k.existingModes(ctx, parent.Changeset, in.Files)
	if err != nil {
		return nil, err
	}
	var changes []object.Change
	uploads := map[object.ID][]byte{}
	for _, f := range in.Files {
		p := strings.Trim(f.Path, "/")
		mode := object.ModeFile
		if f.Executable != nil {
			if *f.Executable {
				mode = object.ModeExec
			}
		} else if modes[p] == object.ModeExec {
			mode = object.ModeExec
		}
		id, stored := object.EncodeObject(object.KindBlob, []byte(f.Content))
		uploads[id] = stored
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	for _, p := range in.Delete {
		changes = append(changes, object.Change{Path: strings.Trim(p, "/")})
	}
	sent, err := k.upload(ctx, uploads)
	if err != nil {
		return nil, err
	}
	var created repo.Created
	err = k.rem.PostJSON(ctx, "/api/changesets", map[string]any{
		"parents": []object.ID{parent.Changeset},
		"author":  orDefault(in.Author, k.author),
		"message": in.Message,
		"changes": changes,
	}, &created)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"changeset":      created.ID,
		"tree":           created.Tree,
		"parent":         parent.Changeset,
		"files_written":  len(in.Files),
		"files_deleted":  len(in.Delete),
		"blobs_uploaded": sent,
	}
	if in.Land != "" {
		res, err := k.land(ctx, created.ID, in.Land)
		if paths, ok := asConflict(err); ok {
			return nil, &conflictError{
				what:  fmt.Sprintf("created changeset %s but could not land it on %s", created.ID, in.Land),
				paths: paths,
				hint:  fmt.Sprintf("Someone else landed overlapping changes to these files. Re-read them at %s, redo your edit on top of them, and call kudzu_write again with parent set to the changeset kudzu_cat reports", in.Land),
			}
		}
		if err != nil {
			return nil, fmt.Errorf("created changeset %s but landing it on %s failed: %w", created.ID, in.Land, err)
		}
		out["land"] = landView(in.Land, res)
	}
	return jsonResult(out), nil
}

// existingModes returns the current modes of the files about to be written,
// so that rewriting an executable keeps its executable bit.
func (k *kudzu) existingModes(ctx context.Context, cs object.ID, files []writeFile) (map[string]uint32, error) {
	modes := map[string]uint32{}
	dirs := map[string]bool{}
	for _, f := range files {
		if f.Executable == nil {
			dirs[path.Dir(strings.Trim(f.Path, "/"))] = true
		}
	}
	for dir := range dirs {
		q := url.Values{"rev": {cs.String()}, "path": {dir}}
		if dir == "." {
			q.Set("path", "")
		}
		var entries []object.Entry
		err := k.rem.GetJSON(ctx, "/api/ls", q, &entries)
		if proto.IsStatus(err, 404) || proto.IsStatus(err, 400) {
			continue // a new directory: every file in it is new
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			p := e.Name
			if dir != "." {
				p = dir + "/" + e.Name
			}
			modes[p] = e.Mode
		}
	}
	return modes, nil
}

// upload sends the blobs the server does not have yet and reports how many.
func (k *kudzu) upload(ctx context.Context, blobs map[object.ID][]byte) (int, error) {
	if len(blobs) == 0 {
		return 0, nil
	}
	ids := make([]object.ID, 0, len(blobs))
	for id := range blobs {
		ids = append(ids, id)
	}
	missing, err := k.rem.Missing(ctx, ids)
	if err != nil {
		return 0, err
	}
	send := make(map[object.ID][]byte, len(missing))
	for _, id := range missing {
		send[id] = blobs[id]
	}
	return len(send), k.rem.Upload(ctx, send)
}

type landIn struct {
	Head string `json:"head" jsonschema:"Changeset to land (ID or prefix)."`
	Ref  string `json:"ref,omitempty" jsonschema:"Ref to land on. Default: main."`
}

func (k *kudzu) landTool(ctx context.Context, in landIn) (*sdk.CallToolResult, error) {
	ref := orDefault(in.Ref, "main")
	head, err := k.resolve(ctx, in.Head)
	if err != nil {
		return nil, err
	}
	res, err := k.land(ctx, head.Changeset, ref)
	if paths, ok := asConflict(err); ok {
		return nil, &conflictError{
			what:  fmt.Sprintf("could not land %s on %s", head.Changeset, ref),
			paths: paths,
			hint:  fmt.Sprintf("Someone else landed overlapping changes to these files. Re-read them at %s and redo the change on top (kudzu_write with parent set to the changeset kudzu_cat reports)", ref),
		}
	}
	if err != nil {
		return nil, err
	}
	return jsonResult(landView(ref, res)), nil
}

type rebaseIn struct {
	Head string `json:"head" jsonschema:"Changeset to rebase (ID or prefix)."`
	Onto string `json:"onto,omitempty" jsonschema:"Revision to rebase onto. Default: main."`
}

func (k *kudzu) rebaseTool(ctx context.Context, in rebaseIn) (*sdk.CallToolResult, error) {
	onto := orDefault(in.Onto, "main")
	var res struct {
		Head object.ID `json:"head"`
		Tree object.ID `json:"tree"`
	}
	err := k.rem.PostJSON(ctx, "/api/rebase", map[string]string{"head": in.Head, "onto": onto}, &res)
	if paths, ok := asConflict(err); ok {
		return nil, &conflictError{
			what:  fmt.Sprintf("could not rebase %s onto %s", in.Head, onto),
			paths: paths,
			hint:  "Nothing was changed. Redo the change on top of the new base instead",
		}
	}
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{"head": res.Head, "tree": res.Tree, "onto": onto}), nil
}
