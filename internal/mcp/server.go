// Package mcp exposes Kudzu to AI agents as a Model Context Protocol server.
// Every tool is backed by the HTTP API through a proto.Remote, so the server
// works against any tier (server, regional cache or node daemon) and never
// needs a checkout. `kudzu mcp` runs it over stdio.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/garyships/kudzu/internal/proto"
)

// Options configures the server.
type Options struct {
	// ReadOnly hides every tool that creates changesets, moves refs or
	// changes a working copy.
	ReadOnly bool
	// Author is the default author of changesets the write tools create.
	Author string
	// Version is reported to clients; it defaults to the module version.
	Version string
}

// Instructions are sent to clients at initialization; clients usually show
// them to the model.
const Instructions = `Kudzu is a version control system for large monorepos. History, blame, diffs, merges and rebases run on the server, so these tools never need a checkout.

Revisions: a ref name (main), a changeset ID or a unique hex prefix of one (4+ characters), optionally followed by ~N for the Nth first-parent ancestor (main~3).

To change files without a checkout: read them (kudzu_cat reports the changeset it read), then call kudzu_write with parent set to that changeset ID and land set to the ref (usually main). Landing rebases onto the current tip on the server; there is no push and no merge queue. A conflict error lists the paths that both you and someone else changed: re-read them at the ref and write again.`

type kudzu struct {
	rem    *proto.Remote
	author string
	wcMu   sync.Mutex // serializes working copy tools
}

// NewServer returns an MCP server whose tools talk to rem.
func NewServer(rem *proto.Remote, opts Options) *sdk.Server {
	if opts.Version == "" {
		opts.Version = moduleVersion()
	}
	if opts.Author == "" {
		opts.Author = "kudzu-mcp <kudzu-mcp@localhost>"
	}
	s := sdk.NewServer(&sdk.Implementation{
		Name:       "kudzu",
		Title:      "Kudzu version control",
		Version:    opts.Version,
		WebsiteURL: "https://github.com/garyships/kudzu",
	}, &sdk.ServerOptions{
		Instructions: Instructions,
		// Tools only, and the tool list never changes: no logging, prompts,
		// resources or list_changed notifications.
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}},
	})
	k := &kudzu{rem: rem, author: opts.Author}
	k.addReadTools(s)
	if !opts.ReadOnly {
		k.addWriteTools(s)
	}
	return s
}

func moduleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

var (
	yes = true
	no  = false
)

// readOnly, additive and destructive are the tool annotations in use.
func readOnly(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &no}
}

func additive(title string, idempotent bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: &no, IdempotentHint: idempotent, OpenWorldHint: &no}
}

func destructive(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: &yes, OpenWorldHint: &no}
}

// addTool registers a tool whose input schema is inferred from In. Unlike
// the SDK's generic AddTool, invalid arguments and handler errors both come
// back as tool errors (isError: true) that the model can read and act on,
// never as protocol errors.
func addTool[In any](s *sdk.Server, t *sdk.Tool, h func(context.Context, In) (*sdk.CallToolResult, error)) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", t.Name, err))
	}
	notNull(schema)
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", t.Name, err))
	}
	t.InputSchema = schema
	if len(schema.Properties) == 0 {
		// Some clients insist on "properties", which the schema omits when empty.
		t.InputSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
	s.AddTool(t, func(ctx context.Context, req *sdk.CallToolRequest) (res *sdk.CallToolResult, _ error) {
		defer func() {
			if r := recover(); r != nil {
				res = errorResult(fmt.Errorf("internal error: %v", r))
			}
		}()
		raw := []byte(req.Params.Arguments)
		if len(raw) == 0 || string(raw) == "null" {
			raw = []byte("{}")
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return errorResult(fmt.Errorf("invalid arguments: %v", err)), nil
		}
		if err := resolved.Validate(generic); err != nil {
			return errorResult(fmt.Errorf("invalid arguments: %v", err)), nil
		}
		var in In
		if err := json.Unmarshal(raw, &in); err != nil {
			return errorResult(fmt.Errorf("invalid arguments: %v", err)), nil
		}
		out, err := h(ctx, in)
		if err != nil {
			return errorResult(err), nil
		}
		return out, nil
	})
}

// notNull rewrites the nullable types inferred for pointer fields to their
// plain type. Optional fields are pointers only so that a missing value can
// mean a non-zero default; null is never a meaningful argument.
func notNull(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) == 2 && s.Types[0] == "null" {
		s.Type, s.Types = s.Types[1], nil
	}
	for _, p := range s.Properties {
		notNull(p)
	}
	notNull(s.Items)
}

func errorResult(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: describeError(err)}},
	}
}

// describeError turns API failures into messages a model can act on.
func describeError(err error) string {
	var he *proto.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict && len(he.Conflicts) > 0 {
		return fmt.Sprintf("%s. Conflicting paths: %s", he.Message, strings.Join(he.Conflicts, ", "))
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, net.ErrClosed) {
		return "cannot reach the kudzu server: " + err.Error()
	}
	return err.Error()
}

// conflictError wraps a 409 from the server with what was already done and
// what to do next.
type conflictError struct {
	what  string // e.g. "could not land abc123 on main"
	paths []string
	hint  string
}

func (e *conflictError) Error() string {
	return fmt.Sprintf("conflict: %s. Conflicting paths: %s. %s", e.what, strings.Join(e.paths, ", "), e.hint)
}

// asConflict returns the conflicting paths of a 409 response.
func asConflict(err error) ([]string, bool) {
	var he *proto.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict {
		return he.Conflicts, true
	}
	return nil, false
}

func jsonText(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf(`{"error": %q}`, err.Error())
	}
	return strings.TrimRight(b.String(), "\n")
}

// jsonResult returns v as JSON text, optionally followed by raw text blocks
// (file contents, patches) that would be hard to read JSON-escaped.
func jsonResult(v any, raw ...string) *sdk.CallToolResult {
	content := []sdk.Content{&sdk.TextContent{Text: jsonText(v)}}
	for _, r := range raw {
		if r != "" {
			content = append(content, &sdk.TextContent{Text: r})
		}
	}
	return &sdk.CallToolResult{Content: content}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func intOr(p *int, def, max int) int {
	if p == nil || *p <= 0 {
		return def
	}
	return min(*p, max)
}
