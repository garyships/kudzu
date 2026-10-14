package mcp

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/textdiff"
)

// Output limits keep tool results small enough for a model's context.
const (
	defaultLogLimit   = 20
	maxLogLimit       = 500
	defaultCatBytes   = 100 << 10
	maxCatBytes       = 1 << 20
	defaultBlameLines = 2000
	defaultPatchBytes = 100 << 10
	maxPatchBytes     = 1 << 20
	maxListed         = 1000 // files in a diff, changes in a changeset
)

func (k *kudzu) addReadTools(s *sdk.Server) {
	addTool(s, &sdk.Tool{
		Name:        "kudzu_resolve",
		Description: "Resolve a revision to its full changeset ID and root tree ID. Use it to pin a moving ref like main before a series of reads, or before kudzu_write so concurrent changes are merged instead of overwritten.",
		Annotations: readOnly("Resolve revision"),
	}, k.resolveTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_log",
		Description: "Show history, newest first: the first-parent history of a revision, or (with path) only the changesets that changed a file or directory. Each entry has the changeset ID, author, date, message and parents.",
		Annotations: readOnly("History"),
	}, k.logTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_blame",
		Description: "Show which changeset last changed each line of a text file (first-parent blame), computed on the server. Returns a JSON summary followed by lines formatted as '<changeset> <author> <date> <line>) <text>'. Use start_line/end_line for large files.",
		Annotations: readOnly("Blame"),
	}, k.blameTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_diff",
		Description: "Compare two revisions on the server. Returns the changed files (status A added, M modified, D deleted) and, unless patch is false, a unified diff after the JSON summary. With only 'to', shows what that changeset changed relative to its first parent. With merge_base, diffs from the common ancestor of from and to (what a branch adds). The patch is truncated to max_patch_bytes.",
		Annotations: readOnly("Diff"),
	}, k.diffTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_cat",
		Description: "Read a file at a revision without a checkout. Returns a JSON summary (resolved changeset, mode, size, whether the content is binary or truncated) followed by the file content. Binary files are not returned. Use start_line/end_line or max_bytes for large files.",
		Annotations: readOnly("Read file"),
	}, k.catTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_ls",
		Description: "List a directory at a revision without a checkout. Each entry has a name and a type (dir, file, executable, symlink or submodule). Listing a file path returns just that file.",
		Annotations: readOnly("List directory"),
	}, k.lsTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_show",
		Description: "Show one changeset: author, date, message, parents, its position in the server's log (seq), its root tree and the files it changed relative to its first parent (deleted files are marked).",
		Annotations: readOnly("Show changeset"),
	}, k.showTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_ancestry",
		Description: "Find the merge base (best common ancestor) of two revisions and whether either is an ancestor of the other, e.g. to check whether a changeset has landed on main.",
		Annotations: readOnly("Merge base and ancestry"),
	}, k.ancestryTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_refs",
		Description: "List refs (named pointers such as main) and the changeset each points at.",
		Annotations: readOnly("List refs"),
	}, k.refsTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_stats",
		Description: "Show statistics of the tier this server is connected to (server, regional cache or node daemon): requests, cache hits and misses, upstream fetches, changesets created and lands.",
		Annotations: readOnly("Tier statistics"),
	}, k.statsTool)
	addTool(s, &sdk.Tool{
		Name:        "kudzu_wc_status",
		Description: "List local changes in a Kudzu working copy on this machine: M modified, D deleted, ? untracked. Also reports the checked-out changeset, the remote and the sparse directories.",
		Annotations: readOnly("Working copy status"),
	}, k.wcStatusTool)
}

type resolved struct {
	Changeset object.ID `json:"changeset"`
	Tree      object.ID `json:"tree"`
}

func (k *kudzu) resolve(ctx context.Context, rev string) (resolved, error) {
	var r resolved
	err := k.rem.GetJSON(ctx, "/api/resolve", url.Values{"rev": {orDefault(rev, "main")}}, &r)
	return r, err
}

func formatDate(t int64, tz int) string {
	return time.Unix(t, 0).In(time.FixedZone("", tz*60)).Format(time.RFC3339)
}

// apiChangeset is a changeset as the HTTP API returns it.
type apiChangeset struct {
	ID          object.ID         `json:"id"`
	Seq         int64             `json:"seq"`
	Tree        object.ID         `json:"tree"`
	Parents     []object.ID       `json:"parents"`
	Author      string            `json:"author"`
	Time        int64             `json:"time"`
	TZ          int               `json:"tz"`
	Message     string            `json:"message"`
	Changes     []object.Change   `json:"changes"`
	Extra       map[string]string `json:"extra"`
	ChangeCount int               `json:"change_count"`
}

type changesetSummary struct {
	ID          object.ID         `json:"id"`
	Author      string            `json:"author"`
	Date        string            `json:"date"`
	Message     string            `json:"message"`
	Parents     []object.ID       `json:"parents"`
	ChangeCount int               `json:"change_count"`
	Extra       map[string]string `json:"extra,omitempty"`
}

func summarize(c apiChangeset) changesetSummary {
	parents := c.Parents
	if parents == nil {
		parents = []object.ID{}
	}
	return changesetSummary{
		ID:          c.ID,
		Author:      c.Author,
		Date:        formatDate(c.Time, c.TZ),
		Message:     strings.TrimRight(c.Message, "\n"),
		Parents:     parents,
		ChangeCount: c.ChangeCount,
		Extra:       c.Extra,
	}
}

type resolveIn struct {
	Rev string `json:"rev" jsonschema:"Revision to resolve: a ref name (e.g. main), a changeset ID or unique hex prefix, optionally with ~N / ^N suffixes (main~3, main^2)."`
}

func (k *kudzu) resolveTool(ctx context.Context, in resolveIn) (*sdk.CallToolResult, error) {
	r, err := k.resolve(ctx, in.Rev)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{"rev": in.Rev, "changeset": r.Changeset, "tree": r.Tree}), nil
}

type logIn struct {
	Rev   string `json:"rev,omitempty" jsonschema:"Revision to start from. Default: main."`
	Path  string `json:"path,omitempty" jsonschema:"Only changesets that changed this file or directory (repository-relative, e.g. services/api)."`
	Limit *int   `json:"limit,omitempty" jsonschema:"Maximum number of changesets (default 20, at most 500)."`
}

func (k *kudzu) logTool(ctx context.Context, in logIn) (*sdk.CallToolResult, error) {
	limit := intOr(in.Limit, defaultLogLimit, maxLogLimit)
	head, err := k.resolve(ctx, in.Rev)
	if err != nil {
		return nil, err
	}
	q := url.Values{"rev": {head.Changeset.String()}, "limit": {fmt.Sprint(limit + 1)}}
	if p := strings.Trim(in.Path, "/"); p != "" {
		q.Set("path", p)
	}
	var cs []apiChangeset
	if err := k.rem.GetJSON(ctx, "/api/log", q, &cs); err != nil {
		return nil, err
	}
	more := len(cs) > limit
	if more {
		cs = cs[:limit]
	}
	out := make([]changesetSummary, len(cs))
	for i, c := range cs {
		out[i] = summarize(c)
	}
	res := map[string]any{"rev": orDefault(in.Rev, "main"), "head": head.Changeset, "changesets": out, "more": more}
	if in.Path != "" {
		res["path"] = strings.Trim(in.Path, "/")
	}
	return jsonResult(res), nil
}

type blameIn struct {
	Path      string `json:"path" jsonschema:"Repository-relative path of a text file."`
	Rev       string `json:"rev,omitempty" jsonschema:"Revision. Default: main."`
	StartLine *int   `json:"start_line,omitempty" jsonschema:"First line to show (1-based). Default: 1."`
	EndLine   *int   `json:"end_line,omitempty" jsonschema:"Last line to show (inclusive). Default: start_line + 1999."`
}

func (k *kudzu) blameTool(ctx context.Context, in blameIn) (*sdk.CallToolResult, error) {
	head, err := k.resolve(ctx, in.Rev)
	if err != nil {
		return nil, err
	}
	var lines []struct {
		Line      int       `json:"line"`
		Changeset object.ID `json:"changeset"`
		Author    string    `json:"author"`
		Time      int64     `json:"time"`
		Text      string    `json:"text"`
	}
	path := strings.Trim(in.Path, "/")
	if err := k.rem.GetJSON(ctx, "/api/blame", url.Values{"rev": {head.Changeset.String()}, "path": {path}}, &lines); err != nil {
		return nil, err
	}
	start, end := lineRange(in.StartLine, in.EndLine, len(lines), defaultBlameLines)
	var b strings.Builder
	for _, l := range lines[start-1 : end] {
		author, _, _ := strings.Cut(l.Author, " <")
		if len(author) > 16 {
			author = author[:16]
		}
		fmt.Fprintf(&b, "%s %-16s %s %4d) %s", l.Changeset.Short(), author, time.Unix(l.Time, 0).UTC().Format("2006-01-02"), l.Line, strings.TrimRight(l.Text, "\n"))
		b.WriteByte('\n')
	}
	summary := map[string]any{
		"path":        path,
		"changeset":   head.Changeset,
		"total_lines": len(lines),
		"start_line":  start,
		"end_line":    end,
		"truncated":   start > 1 || end < len(lines),
	}
	if len(lines) == 0 {
		summary["start_line"], summary["end_line"] = 0, 0
	}
	return jsonResult(summary, b.String()), nil
}

// lineRange clamps a 1-based inclusive range to n lines, showing at most
// def lines when no end is given. For n == 0 it returns (1, 0).
func lineRange(startP, endP *int, n, def int) (int, int) {
	start := 1
	if startP != nil && *startP > 1 {
		start = *startP
	}
	end := start + def - 1
	if endP != nil && *endP > 0 {
		end = *endP
	}
	end = min(end, n)
	start = min(start, end+1)
	if start < 1 {
		start = 1
	}
	return start, end
}

type diffIn struct {
	To            string `json:"to,omitempty" jsonschema:"New revision. Default: main."`
	From          string `json:"from,omitempty" jsonschema:"Old revision. Default: the first parent of 'to'."`
	MergeBase     bool   `json:"merge_base,omitempty" jsonschema:"Diff from the merge base of from and to instead of from itself (what 'to' adds since it forked)."`
	Path          string `json:"path,omitempty" jsonschema:"Restrict the diff to this file or directory."`
	Patch         *bool  `json:"patch,omitempty" jsonschema:"Include a unified diff. Default: true. Set false to list changed files only."`
	MaxPatchBytes *int   `json:"max_patch_bytes,omitempty" jsonschema:"Truncate the patch after this many bytes (default 102400, at most 1048576)."`
}

type fileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	OldMode string `json:"old_mode,omitempty"`
	NewMode string `json:"new_mode,omitempty"`
}

func fileStatus(d repo.FileDiff) fileChange {
	fc := fileChange{Path: d.Path, Status: "M"}
	if d.Old == nil {
		fc.Status = "A"
	} else if d.New == nil {
		fc.Status = "D"
	}
	if d.Old != nil && d.New != nil && d.Old.Mode != d.New.Mode {
		fc.OldMode, fc.NewMode = fmt.Sprintf("%o", d.Old.Mode), fmt.Sprintf("%o", d.New.Mode)
	}
	return fc
}

func (k *kudzu) diffTool(ctx context.Context, in diffIn) (*sdk.CallToolResult, error) {
	wantPatch := in.Patch == nil || *in.Patch
	q := url.Values{"to": {orDefault(in.To, "main")}}
	if in.From != "" {
		q.Set("from", in.From)
	}
	if in.MergeBase {
		if in.From == "" {
			return nil, fmt.Errorf("merge_base needs 'from'")
		}
		q.Set("merge_base", "1")
	}
	if p := strings.Trim(in.Path, "/"); p != "" {
		q.Set("path", p)
	}
	if wantPatch {
		q.Set("patch", "1")
	}
	var resp struct {
		From  object.ID       `json:"from"`
		To    object.ID       `json:"to"`
		Files []repo.FileDiff `json:"files"`
		Patch string          `json:"patch"`
	}
	if err := k.rem.GetJSON(ctx, "/api/diff", q, &resp); err != nil {
		return nil, err
	}
	files := make([]fileChange, 0, min(len(resp.Files), maxListed))
	for _, f := range resp.Files[:min(len(resp.Files), maxListed)] {
		files = append(files, fileStatus(f))
	}
	summary := map[string]any{
		"from":        resp.From,
		"to":          resp.To,
		"files":       files,
		"total_files": len(resp.Files),
	}
	if len(resp.Files) > maxListed {
		summary["files_truncated"] = true
	}
	patch := ""
	if wantPatch {
		limit := intOr(in.MaxPatchBytes, defaultPatchBytes, maxPatchBytes)
		var truncated bool
		patch, truncated = truncateText(resp.Patch, limit)
		summary["patch_bytes"] = len(resp.Patch)
		summary["patch_truncated"] = truncated
		if truncated {
			summary["note"] = fmt.Sprintf("patch truncated to %d of %d bytes; narrow it with path or raise max_patch_bytes", len(patch), len(resp.Patch))
		}
	}
	return jsonResult(summary, patch), nil
}

// truncateText cuts s to at most limit bytes, at a line boundary when one is
// reasonably close, and never inside a UTF-8 sequence.
func truncateText(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl >= cut/2 {
		cut = nl + 1
	}
	return s[:cut], true
}

type catIn struct {
	Path      string `json:"path" jsonschema:"Repository-relative path of the file."`
	Rev       string `json:"rev,omitempty" jsonschema:"Revision. Default: main."`
	StartLine *int   `json:"start_line,omitempty" jsonschema:"First line to return (1-based)."`
	EndLine   *int   `json:"end_line,omitempty" jsonschema:"Last line to return (inclusive)."`
	MaxBytes  *int   `json:"max_bytes,omitempty" jsonschema:"Truncate the returned content after this many bytes (default 102400, at most 1048576)."`
}

func (k *kudzu) catTool(ctx context.Context, in catIn) (*sdk.CallToolResult, error) {
	head, err := k.resolve(ctx, in.Rev)
	if err != nil {
		return nil, err
	}
	path := strings.Trim(in.Path, "/")
	var entries []object.Entry
	if err := k.rem.GetJSON(ctx, "/api/ls", url.Values{"rev": {head.Changeset.String()}, "path": {path}}, &entries); err != nil {
		return nil, err
	}
	if len(entries) != 1 || entries[0].IsDir() || path == "" {
		return nil, fmt.Errorf("%s is a directory; use kudzu_ls", orDefault(path, "/"))
	}
	data, err := k.rem.GetRaw(ctx, "/api/cat", url.Values{"rev": {head.Changeset.String()}, "path": {path}})
	if err != nil {
		return nil, err
	}
	summary := map[string]any{
		"path":      path,
		"changeset": head.Changeset,
		"mode":      fmt.Sprintf("%o", entries[0].Mode),
		"type":      entryType(entries[0].Mode),
		"size":      len(data),
		"binary":    false,
		"truncated": false,
	}
	if textdiff.IsBinary(data) {
		summary["binary"] = true
		summary["note"] = "binary file; content not shown"
		return jsonResult(summary), nil
	}
	text := strings.ToValidUTF8(string(data), "�")
	truncated := false
	if in.StartLine != nil || in.EndLine != nil {
		lines := textdiff.SplitLines([]byte(text))
		start, end := lineRange(in.StartLine, in.EndLine, len(lines), len(lines))
		text = strings.Join(lines[start-1:end], "")
		summary["start_line"], summary["end_line"], summary["total_lines"] = start, end, len(lines)
		truncated = start > 1 || end < len(lines)
	}
	text, cut := truncateText(text, intOr(in.MaxBytes, defaultCatBytes, maxCatBytes))
	if cut {
		summary["note"] = fmt.Sprintf("content truncated to %d bytes; use start_line/end_line to read the rest", len(text))
	}
	summary["truncated"] = truncated || cut
	if text == "" {
		return jsonResult(summary), nil
	}
	return jsonResult(summary, text), nil
}

func entryType(mode uint32) string {
	switch mode {
	case object.ModeDir:
		return "dir"
	case object.ModeExec:
		return "executable"
	case object.ModeSymlink:
		return "symlink"
	case object.ModeGitlink:
		return "submodule"
	}
	return "file"
}

type lsIn struct {
	Path string `json:"path,omitempty" jsonschema:"Directory to list (repository-relative). Default: the root."`
	Rev  string `json:"rev,omitempty" jsonschema:"Revision. Default: main."`
}

func (k *kudzu) lsTool(ctx context.Context, in lsIn) (*sdk.CallToolResult, error) {
	head, err := k.resolve(ctx, in.Rev)
	if err != nil {
		return nil, err
	}
	path := strings.Trim(in.Path, "/")
	var entries []object.Entry
	if err := k.rem.GetJSON(ctx, "/api/ls", url.Values{"rev": {head.Changeset.String()}, "path": {path}}, &entries); err != nil {
		return nil, err
	}
	type entry struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	out := make([]entry, len(entries))
	for i, e := range entries {
		out[i] = entry{e.Name, entryType(e.Mode)}
	}
	return jsonResult(map[string]any{"path": orDefault(path, "/"), "changeset": head.Changeset, "entries": out}), nil
}

type showIn struct {
	Rev string `json:"rev" jsonschema:"Changeset to show: a changeset ID or prefix, or a ref name (e.g. main), optionally with ~N / ^N suffixes."`
}

func (k *kudzu) showTool(ctx context.Context, in showIn) (*sdk.CallToolResult, error) {
	var c apiChangeset
	if err := k.rem.GetJSON(ctx, "/api/changesets/"+url.PathEscape(in.Rev), nil, &c); err != nil {
		return nil, err
	}
	type change struct {
		Path    string `json:"path"`
		Mode    string `json:"mode,omitempty"`
		Deleted bool   `json:"deleted,omitempty"`
	}
	changes := make([]change, 0, min(len(c.Changes), maxListed))
	for _, ch := range c.Changes[:min(len(c.Changes), maxListed)] {
		if ch.Deleted() {
			changes = append(changes, change{Path: ch.Path, Deleted: true})
		} else {
			changes = append(changes, change{Path: ch.Path, Mode: fmt.Sprintf("%o", ch.Mode)})
		}
	}
	out := map[string]any{
		"changeset": summarize(c),
		"seq":       c.Seq,
		"tree":      c.Tree,
		"changes":   changes,
	}
	if len(c.Changes) > maxListed {
		out["changes_truncated"] = true
	}
	return jsonResult(out), nil
}

type ancestryIn struct {
	A string `json:"a" jsonschema:"First revision."`
	B string `json:"b" jsonschema:"Second revision."`
}

func (k *kudzu) ancestryTool(ctx context.Context, in ancestryIn) (*sdk.CallToolResult, error) {
	var resp map[string]any
	if err := k.rem.GetJSON(ctx, "/api/ancestry", url.Values{"a": {in.A}, "b": {in.B}}, &resp); err != nil {
		return nil, err
	}
	if resp["merge_base"] == "" {
		resp["note"] = "no common ancestor"
	}
	return jsonResult(resp), nil
}

type refsIn struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"Only refs whose name starts with this prefix."`
}

func (k *kudzu) refsTool(ctx context.Context, in refsIn) (*sdk.CallToolResult, error) {
	var refs map[string]object.ID
	if err := k.rem.GetJSON(ctx, "/api/refs", nil, &refs); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		if strings.HasPrefix(name, in.Prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type ref struct {
		Name      string    `json:"name"`
		Changeset object.ID `json:"changeset"`
	}
	out := make([]ref, len(names))
	for i, n := range names {
		out[i] = ref{n, refs[n]}
	}
	return jsonResult(map[string]any{"refs": out}), nil
}

type statsIn struct{}

func (k *kudzu) statsTool(ctx context.Context, _ statsIn) (*sdk.CallToolResult, error) {
	var stats map[string]any
	if err := k.rem.GetJSON(ctx, "/stats", nil, &stats); err != nil {
		return nil, err
	}
	return jsonResult(stats), nil
}
