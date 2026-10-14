package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"os"

	"github.com/garyships/kudzu/internal/client"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
)

// jsonOutput is set by --json on commands that support it. Their result is
// then a single JSON document on stdout; a failure is a JSON object on
// stderr ({"error": ..., "status": ..., "conflicts": [...]}) and exit code 1.
var jsonOutput bool

func addJSONFlag(fs *flag.FlagSet) {
	fs.BoolVar(&jsonOutput, "json", false, "print the result as JSON")
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printJSON(v any) error { return writeJSON(os.Stdout, v) }

// printJSONError reports err as JSON on stderr.
func printJSONError(err error) {
	out := map[string]any{"error": err.Error()}
	var he *proto.HTTPError
	if errors.As(err, &he) {
		out["error"] = he.Message
		out["status"] = he.Status
		if len(he.Conflicts) > 0 {
			out["conflicts"] = he.Conflicts
		}
	}
	writeJSON(os.Stderr, out)
}

// query GETs a JSON API endpoint. With --json it prints the response as is
// and reports done; otherwise it decodes the response into out.
func query(ctx context.Context, rem *proto.Remote, path string, q url.Values, out any) (done bool, err error) {
	if !jsonOutput {
		return false, rem.GetJSON(ctx, path, q, out)
	}
	var raw json.RawMessage
	if err := rem.GetJSON(ctx, path, q, &raw); err != nil {
		return false, err
	}
	return true, printJSON(raw)
}

// landJSON is a land result as `land --json` and `commit --json` print it.
type landJSON struct {
	Ref string `json:"ref"`
	*repo.LandResult
}

type fileStatusJSON struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type statusJSON struct {
	Changeset object.ID        `json:"changeset"`
	Files     []fileStatusJSON `json:"files"`
}

// localDiffJSON prints the working copy's uncommitted changes to tracked
// files: {"files": [{"path", "status"}], "patch": "..."}.
func localDiffJSON(ctx context.Context, wc *client.WC, statOnly bool) error {
	st, err := wc.Status(ctx)
	if err != nil {
		return err
	}
	out := struct {
		Files []fileStatusJSON `json:"files"`
		Patch string           `json:"patch,omitempty"`
	}{Files: []fileStatusJSON{}}
	for _, f := range st {
		if f.Code != '?' {
			out.Files = append(out.Files, fileStatusJSON{f.Path, string(f.Code)})
		}
	}
	if !statOnly {
		if out.Patch, err = wc.LocalDiff(ctx); err != nil {
			return err
		}
	}
	return printJSON(out)
}
