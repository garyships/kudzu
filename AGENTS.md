# AGENTS.md

Guidance for AI coding agents (and humans) working on the Kudzu codebase. To *use* Kudzu from an agent instead, see [skills/kudzu/SKILL.md](skills/kudzu/SKILL.md) and [docs/AI.md](docs/AI.md).

## What Kudzu is

A version control system for monorepos whose clients are mostly always-online agents and CI jobs.
History, blame, diff, rebase and land run on the server; clients fetch only the trees and blobs they ask for (sparse by default) through tiers of caches.
Commits are changesets appended to a log; refs move by compare-and-swap, so landing never takes a lock.

## Repo map

| Path | Responsibility |
|---|---|
| `cmd/kudzu/` | The single binary. `main.go` holds the command table; `infra.go` has `server`, `cache`, `daemon`, `import`, `snapshot`, `stats`; `client.go` has the working-copy, query and `write` commands; `jsonout.go` the `--json` helpers; `mcp.go` is `kudzu mcp`. |
| `internal/object/` | Content-addressed objects (blob, tree, unode), changesets, IDs and the stored form used on disk and on the wire. |
| `internal/store/` | Storage interfaces: `Meta` (SQLite: changeset log, refs with CAS, derived data, git mapping) and `Objects` (`FS`, in-memory `Mem`, layered `Cached`). |
| `internal/repo/` | Server-side core: derived-data engine (`derive.go`), revisions, skip list and merge base (`ancestry.go`), log/blame/diff/cat/ls (`query.go`), tree edits (`edits.go`), create, three-way merge, rebase, merge and lock-free land (`write.go`). |
| `internal/server/` | The authoritative tier: the HTTP API (`/api/*`) and the object protocol. The API table is at the end of [docs/DESIGN.md](docs/DESIGN.md). |
| `internal/proto/` | Object protocol framing and `Remote`, the HTTP client every other package uses (`host:port`, `http(s)://…` or `unix:///path`). |
| `internal/tier/` | Regional cache and node daemon (same code): coalesced misses, bounded upstream concurrency, snapshot seeding, `/stats`. |
| `internal/client/` | The working copy: clone, status, commit, update, sparse, land, rebase, local diff. |
| `internal/mcp/` | MCP server (`kudzu mcp`): tools backed by the HTTP API, plus a few working-copy tools. |
| `internal/sparse/` | Cone-mode sparse rules shared by client and server. |
| `internal/textdiff/` | Myers diff, unified diff output, line-level three-way merge. |
| `internal/gitimport/` | Incremental one-way import from git. |
| `internal/lru/` | Small generic LRU. |
| `internal/e2e/` | Full-stack tests: server → cache → daemon (unix socket) → working copies, all in one process. |
| `scripts/demo.sh` | End-to-end demo against a real git repository. `scripts/record-demo/` renders the website's demo video. |
| `docs/` | [DESIGN.md](docs/DESIGN.md) (English) and [DESIGN.zh-CN.md](docs/DESIGN.zh-CN.md), agent docs ([AI.md](docs/AI.md), [llms.txt](docs/llms.txt), [llms-full.txt](docs/llms-full.txt)) and the GitHub Pages site (`index.html`, `assets/`). |
| `skills/kudzu/` | Agent Skill that teaches an agent to use Kudzu. |

## Build and test

Go 1.24+. `git` is only needed for `kudzu import` and its tests.

```bash
go build -o kudzu ./cmd/kudzu      # the binary (ignored by .gitignore)
gofmt -l .                         # must print nothing
go vet ./...
go test -race ./...                # what CI runs, together with the two above
go test -race ./internal/mcp/      # one package
```

Run the whole stack by hand:

```bash
./kudzu import --store /tmp/kd --git . --as main
./kudzu server --store /tmp/kd --listen 127.0.0.1:7070 &
KUDZU_REMOTE=127.0.0.1:7070 ./kudzu log -n 5 --oneline
```

`scripts/demo.sh /path/to/git/repo [sparse/dir]` builds the binary, imports the repository, starts all three tiers, then clones, commits, lands concurrently and prints cache statistics. It needs `bc`.

## Conventions

- Keep `gofmt`, `go vet` and `go test -race ./...` clean. New behavior gets a test; cross-tier behavior belongs in `internal/e2e`, which shows how to start the whole stack in-process.
- No new dependencies without a strong reason. Today there are two direct ones: `modernc.org/sqlite` (pure Go, no cgo) and the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`, plus its `jsonschema-go`). Keep `go.mod` buildable with Go 1.24.
- The client stays thin: history, blame, merges and rebases belong on the server (`internal/repo`), exposed through `internal/server` and called through `proto.Remote`.
- Objects are immutable and content-addressed, and the stored form is the same on every tier. Don't add per-tier formats.
- Commands print human-readable output by default. Query and commit commands also take `--json`; when you add output, add it to the JSON form too and keep stdout free of anything else.
- `kudzu mcp` speaks the protocol on stdout: diagnostics go to stderr, and the working-copy tools set `wc.Out = io.Discard`.
- Errors that reach users name the thing that failed. Conflicts carry the conflicting paths (`repo.ConflictError` → HTTP 409 → `proto.HTTPError.Conflicts`).
- Remote resolution everywhere: `--remote` > `$KUDZU_REMOTE` > the working copy's recorded remote. Author: `--author` > `$KUDZU_AUTHOR` > `user <user@host>`.

## Design

[docs/DESIGN.md](docs/DESIGN.md) explains the data model, the derived-data engine, the object protocol, the cache tiers, the client, and which choices are Kudzu's own rather than the source article's. Read it before changing anything in `internal/repo`, `internal/proto` or `internal/tier`.
