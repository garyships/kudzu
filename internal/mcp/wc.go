package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/garyships/kudzu/internal/client"
)

// Working copy tools act on a checkout on the machine running the MCP server
// and talk to the remote recorded in that checkout.

type wcIn struct {
	Dir string `json:"dir,omitempty" jsonschema:"Directory inside the working copy. Use an absolute path; relative paths resolve against the MCP server's working directory. Default: that directory."`
}

func openWC(dir string) (*client.WC, error) {
	wc, err := client.Open(orDefault(dir, "."))
	if err != nil {
		return nil, err
	}
	wc.Out = io.Discard // stdout carries the MCP protocol
	return wc, nil
}

func (k *kudzu) wcStatusTool(ctx context.Context, in wcIn) (*sdk.CallToolResult, error) {
	k.wcMu.Lock()
	defer k.wcMu.Unlock()
	wc, err := openWC(in.Dir)
	if err != nil {
		return nil, err
	}
	st, err := wc.Status(ctx)
	if err != nil {
		return nil, err
	}
	type file struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	files := make([]file, len(st))
	for i, f := range st {
		files[i] = file{f.Path, string(f.Code)}
	}
	sparse := []string(wc.State.Sparse)
	if sparse == nil {
		sparse = []string{}
	}
	return jsonResult(map[string]any{
		"root":      wc.Root,
		"remote":    wc.State.Remote,
		"changeset": wc.State.Changeset,
		"sparse":    sparse,
		"files":     files,
	}), nil
}

type wcCommitIn struct {
	Dir          string `json:"dir,omitempty" jsonschema:"Directory inside the working copy (absolute path recommended)."`
	Message      string `json:"message" jsonschema:"Changeset message."`
	AddUntracked bool   `json:"add_untracked,omitempty" jsonschema:"Also commit untracked files (like 'kudzu commit -A')."`
	Land         string `json:"land,omitempty" jsonschema:"Ref to land the new changeset on, e.g. main. Default: do not land."`
	Author       string `json:"author,omitempty" jsonschema:"Author as 'Name <email>'. Default: the author kudzu mcp was started with (--author, $KUDZU_AUTHOR or user@host)."`
}

func (k *kudzu) wcCommitTool(ctx context.Context, in wcCommitIn) (*sdk.CallToolResult, error) {
	if strings.TrimSpace(in.Message) == "" {
		return nil, errors.New("message must not be empty")
	}
	k.wcMu.Lock()
	defer k.wcMu.Unlock()
	wc, err := openWC(in.Dir)
	if err != nil {
		return nil, err
	}
	res, err := wc.Commit(ctx, in.Message, orDefault(in.Author, k.author), in.AddUntracked, in.Land)
	if paths, ok := asConflict(err); ok {
		return nil, &conflictError{
			what:  fmt.Sprintf("could not land on %s", in.Land),
			paths: paths,
			hint: "The working copy is unchanged and still holds your edits. To resolve: commit without land (keeps your change as changeset X), " +
				"update to " + in.Land + ", re-apply your change on top (kudzu_diff with to=X shows it), then commit again with land",
		}
	}
	if errors.Is(err, client.ErrNothingToCommit) {
		return nil, errors.New("nothing to commit: no tracked file changed (set add_untracked to include new files)")
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"changeset": res.ID, "tree": res.Tree, "changes": res.Changes, "now_at": wc.State.Changeset}
	if res.Land != nil {
		out["land"] = landView(in.Land, res.Land)
	}
	return jsonResult(out), nil
}

type wcUpdateIn struct {
	Dir string `json:"dir,omitempty" jsonschema:"Directory inside the working copy (absolute path recommended)."`
	Rev string `json:"rev,omitempty" jsonschema:"Revision to move to. Default: main."`
}

func (k *kudzu) wcUpdateTool(ctx context.Context, in wcUpdateIn) (*sdk.CallToolResult, error) {
	k.wcMu.Lock()
	defer k.wcMu.Unlock()
	wc, err := openWC(in.Dir)
	if err != nil {
		return nil, err
	}
	before := wc.State.Changeset
	if err := wc.Update(ctx, orDefault(in.Rev, "main"), false); err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{"previous": before, "changeset": wc.State.Changeset, "tree": wc.State.Tree}), nil
}
