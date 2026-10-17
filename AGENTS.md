# AGENTS.md

Guidance for AI coding agents (and humans) working on the Kudzu codebase. To *use* Kudzu from an agent instead, see [skills/kudzu/SKILL.md](skills/kudzu/SKILL.md) and [docs/AI.md](docs/AI.md).

## What Kudzu is

A version control system for monorepos whose clients are mostly always-online agents and CI jobs.
History, blame, diff, rebase and land run on the server; clients fetch only the trees and blobs they ask for (sparse by default) through tiers of caches.
Commits are changesets appended to a log; refs move by compare-and-swap, so landing never takes a lock.

## Repo map

| Path | Responsibility |
|---|---|
| `cmd/kudzu/` | The single binary. `main.go` holds the command table; `up.go` is the one-process playground `kudzu up`; `infra.go` has `server`, `cache`, `daemon`, `import`, `export`, `snapshot`, `stats`; `client.go` has the working-copy, query and `write` commands; `jsonout.go` the `--json` helpers; `format.go` plural and display-width helpers; `mcp.go` is `kudzu mcp`. |
| `internal/object/` | Content-addressed objects (blob, tree, unode), changesets, IDs and the stored form used on disk and on the wire. |
| `internal/store/` | Storage interfaces: `Meta` (changeset log, refs with CAS, derived data, git mapping; SQLite or Postgres) and `Objects` (`FS`, in-memory `Mem`, layered `Cached`, S3-compatible `S3`). `conformance_test.go` holds the tests every backend must pass; `storetest` picks the backends repo and e2e tests use. |
| `internal/repo/` | Server-side core: derived-data engine (`derive.go`), revisions, skip list and merge base (`ancestry.go`), log/blame/diff/cat/ls (`query.go`), tree edits (`edits.go`), create, three-way merge, rebase, merge and lock-free land (`write.go`), mark-and-sweep garbage collection (`gc.go`). |
| `internal/server/` | The authoritative tier: the HTTP API (`/api/*`) and the object protocol. The API table is at the end of [docs/DESIGN.md](docs/DESIGN.md). `views.go` filters trees for callers with restricted read access. |
| `internal/auth/` | Bearer tokens, principals and per-directory read/write/land rules (`kudzu server --auth`). |
| `internal/proto/` | Object protocol framing and `Remote`, the HTTP client every other package uses (`host:port`, `http(s)://…` or `unix:///path`). |
| `internal/tier/` | Regional cache and node daemon (same code): coalesced misses, bounded upstream concurrency, snapshot seeding, `/stats`. |
| `internal/client/` | The working copy: clone, status, commit, update, sparse, land, rebase, local diff. |
| `internal/vfs/` | A lazily fetched, read-only view of one revision (the core of `kudzu mount`). |
| `internal/fusefs/` | FUSE adapter for `vfs` (go-fuse); built only with `-tags fuse`, like `cmd/kudzu/mount_fuse.go`. |
| `internal/mcp/` | MCP server (`kudzu mcp`): tools backed by the HTTP API, plus a few working-copy tools. |
| `internal/sparse/` | Cone-mode sparse rules shared by client and server. |
| `internal/textdiff/` | Myers diff, unified diff output, line-level three-way merge. |
| `internal/gitimport/` | Incremental one-way import from git. |
| `internal/gitexport/` | Incremental export of a ref's history to a git repository through `git fast-import`. |
| `internal/lru/` | Small generic LRU. |
| `internal/e2e/` | Full-stack tests: server → cache → daemon (unix socket) → working copies, all in one process. |
| `install.sh`, `.goreleaser.yaml` | The curl installer and the release build; pushing a `v*` tag runs `.github/workflows/release.yml`. |
| `scripts/demo.sh` | End-to-end demo against a real git repository. `scripts/record-demo/` renders the website's demo video. |
| `docs/` | [DESIGN.md](docs/DESIGN.md) (English) and [DESIGN.zh-CN.md](docs/DESIGN.zh-CN.md), agent docs ([AI.md](docs/AI.md), [llms.txt](docs/llms.txt), [llms-full.txt](docs/llms-full.txt)) and the GitHub Pages site (`index.html`, `assets/`). |
| `skills/kudzu/` | Agent Skill that teaches an agent to use Kudzu. |

## Build and test

Go 1.26+ (the oldest release that still gets security fixes). `git` is only needed for `kudzu import`, `kudzu export` and their tests.

```bash
go build -o kudzu ./cmd/kudzu      # the binary (ignored by .gitignore)
gofmt -l .                         # must print nothing
go vet ./...
go test -race ./...                # what CI runs, together with the two above, on Go 1.26.x and stable
staticcheck ./...                  # CI also runs staticcheck 2026.2.1 and govulncheck
go test -race ./internal/mcp/      # one package
go vet -tags fuse ./... && go test -tags fuse ./internal/fusefs/   # kudzu mount (needs /dev/fuse)
```

To run the repo, e2e and store tests against shared backends, point them at a Postgres database and an S3-compatible bucket (each test uses its own schema or key prefix):

```bash
KUDZU_TEST_POSTGRES=postgres://postgres:pw@127.0.0.1:5432/postgres?sslmode=disable \
KUDZU_TEST_S3='s3://kudzu-test?endpoint=http://127.0.0.1:9000' AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… \
go test ./internal/store/... ./internal/repo/ ./internal/e2e/
```

Run the whole stack in one process with `go run ./cmd/kudzu up` (imports this repository and serves on `unix:///tmp/kudzu.sock`), or by hand:

```bash
./kudzu import --store /tmp/kd --git . --as main
./kudzu server --store /tmp/kd --listen 127.0.0.1:7070 &
KUDZU_REMOTE=127.0.0.1:7070 ./kudzu log -n 5 --oneline
```

`scripts/demo.sh /path/to/git/repo [sparse/dir]` builds the binary, imports the repository, starts all three tiers, then clones, commits, lands concurrently and prints cache statistics. It needs `bc`.

## Conventions

- Keep `gofmt`, `go vet`, `staticcheck` and `go test -race ./...` clean. CI also runs every test on macOS and builds, vets and tests the platform-neutral packages on Windows, so guard OS-specific code with build tags (`GOOS=windows go vet ./...` checks it locally). New behavior gets a test; cross-tier behavior belongs in `internal/e2e`, which shows how to start the whole stack in-process.
- No new dependencies without a strong reason. Today the direct ones are `modernc.org/sqlite` (pure Go, no cgo), `github.com/jackc/pgx/v5` (Postgres) and the official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`, plus its `jsonschema-go`). S3 is spoken directly (`store/s3.go`), not through an SDK. Keep `go.mod` on the oldest Go release that still gets security fixes (1.26 today), and its modules free of what govulncheck reports.
- The client stays thin: history, blame, merges and rebases belong on the server (`internal/repo`), exposed through `internal/server` and called through `proto.Remote`.
- Objects are immutable and content-addressed, and the stored form is the same on every tier. Don't add per-tier formats.
- Commands print human-readable output by default. Query and commit commands also take `--json`; when you add output, add it to the JSON form too and keep stdout free of anything else.
- `kudzu mcp` speaks the protocol on stdout: diagnostics go to stderr, and the working-copy tools set `wc.Out = io.Discard`.
- Errors that reach users name the thing that failed. Conflicts carry the conflicting paths (`repo.ConflictError` → HTTP 409 → `proto.HTTPError.Conflicts`).
- Remote resolution everywhere: `--remote` > `$KUDZU_REMOTE` > the working copy's recorded remote. Author: `--author` > `$KUDZU_AUTHOR` > `user <user@host>`. Token: `$KUDZU_TOKEN`, read by `proto.Dial` and never persisted.
- New `/api` handlers that read a path check `auth.FromContext(ctx)` (a nil principal means auth is off), and any tree ID they return goes through `Server.viewTree`.

## Design

[docs/DESIGN.md](docs/DESIGN.md) explains the data model, the derived-data engine, the object protocol, the cache tiers, the client, and which choices are Kudzu's own rather than the source article's. Read it before changing anything in `internal/repo`, `internal/proto` or `internal/tier`.
