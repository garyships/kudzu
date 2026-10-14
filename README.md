<div align="center">

# Kudzu

### Anthropic's monorepo outgrew git at 30,000+ merges a day, so they built Scion and kept it internal.<br/>Kudzu is the open-source rebuild.

Version control for monorepos where your committers are AI agents.<br/>
Rebuilt from Anthropic's [engineering post](https://www.anthropic.com/engineering/redesigning-source-control) the day after it was published.<br/>
*Your agents write code like kudzu grows. Your version control should keep up.*

[![ci](https://github.com/garyships/kudzu/actions/workflows/ci.yml/badge.svg)](https://github.com/garyships/kudzu/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-3fb950)](LICENSE)
![go 1.24+](https://img.shields.io/badge/go-1.24%2B-00ADD8)

[Website](https://garyships.github.io/kudzu/) · [Demo video](https://garyships.github.io/kudzu/#demo) · [Design](docs/DESIGN.md) · [For AI agents](docs/AI.md) · [中文](README.zh-CN.md)

<img src="docs/assets/demo.gif" alt="Kudzu demo: import git history, sparse clone, concurrent lock-free lands" width="860">

</div>

## Why not git?

git gives every clone the whole repository so it can work offline. That's the wrong trade when nearly every client is an always-online agent and a monorepo merges tens of thousands of changes a day. Kudzu moves the heavy lifting to the server and lets clients fetch only what they need.

| | git | Kudzu |
|---|---|---|
| **Clone** | full history to every client | only the trees and blobs you ask for, sparse by default |
| **History, blame, diff** | computed from your local clone | server API, no checkout needed |
| **Commit** | commit locally, then push | the server creates the changeset; nothing to push |
| **Landing** | merge queue, global lock | lock-free: server-side rebase plus compare-and-swap on the ref |
| **Scaling reads** | mirrors that each hold the whole repo | tiers of caches for immutable objects |
| **Maintenance** | repack and gc on every replica | none: no packfiles |
| **Offline** | ✅ fully | ❌ needs the server, by design |
| **Storage** | delta-compressed packs (smaller) | one object per blob, no deltas (larger) |

## How it works

```mermaid
flowchart TB
  S["kudzu server<br/>changeset log · refs · derived data<br/>history · blame · diff · rebase · land"]
  CA["kudzu cache · region A<br/>in memory, coalesces misses"]
  CB["kudzu cache · region B"]
  D1["kudzu daemon · node 1<br/>on disk, shared by every job"]
  D2["kudzu daemon · node 2"]
  D3["kudzu daemon · node 3"]
  S --- CA & CB
  CA --- D1 & D2
  CB --- D3
  D1 --- A1(["agent"]) & A2(["agent"])
  D2 --- A3(["CI job"])
  D3 --- A4(["agent"])
```

Every tier answers the **same object RPCs**, so clients never need to know the topology. Objects are content-addressed and immutable, so any tier can serve any request.

**Clone = ask for what you want, never say what you have.**

```mermaid
sequenceDiagram
  participant A as agent
  participant D as node daemon
  participant C as regional cache
  participant S as server
  A->>S: resolve main (proxied through the tiers)
  S-->>A: changeset + root tree id
  A->>D: want [root tree]
  D-->>A: hit
  A->>D: want [services/, docs/]  (subtrees outside the sparse cone are never requested)
  D->>C: miss: want [docs/]
  C-->>D: hit, served from memory
  D-->>A: trees
  A->>D: want [blobs…] in batches, streamed and resumable
```

**Landing never takes a lock.**

```mermaid
sequenceDiagram
  participant A1 as agent 1
  participant S as server
  participant A2 as agent 2
  A1->>S: commit --land main
  A2->>S: commit --land main
  S->>S: CAS main m0 → c1 ✓
  S->>S: CAS main m0 → c2 ✗ lost the race
  S->>S: rebase c2 onto c1 = c2′, CAS c1 → c2′ ✓
  S-->>A1: landed c1
  S-->>A2: landed c2′ (rebased on the server)
```

Changesets record only what changed. Root trees, per-path history, a commit-graph skip list and blame are **derived data** that one recursive engine computes, eagerly or lazily. See [docs/DESIGN.md](docs/DESIGN.md) for the details.

## Quick start

```bash
go install github.com/garyships/kudzu/cmd/kudzu@latest

# import an existing git repo (re-run any time; only new commits are ingested)
kudzu import --store ./data --git /path/to/repo --as main

# run the three tiers
kudzu server --store ./data --listen 127.0.0.1:7070 &
kudzu cache  --upstream 127.0.0.1:7070 --listen 127.0.0.1:7071 &
kudzu daemon --upstream 127.0.0.1:7071 --listen unix:///tmp/kudzu.sock &
export KUDZU_REMOTE=unix:///tmp/kudzu.sock

# history without a checkout
kudzu log -n 5 --oneline
kudzu blame src/server.go
kudzu diff --from main~3 --to main --stat

# sparse clone, edit, land
kudzu clone --sparse services/api $KUDZU_REMOTE wc && cd wc
echo "// hi" >> services/api/handler.go
kudzu commit -m "api: say hi" --land main

# or skip the checkout entirely
kudzu write -r main -m "bump version" --land main VERSION=@./VERSION
```

`scripts/demo.sh /path/to/git/repo` runs the whole flow end to end and prints each tier's cache statistics.

## Use it from an AI agent

Kudzu ships three ways for agents to drive it:

- **MCP server**: `kudzu mcp` exposes 16 tools, including log, blame, diff, cat, and write + land without a checkout. `--read-only` hides the write tools.
- **Agent Skill**: [`skills/kudzu`](skills/kudzu/SKILL.md) teaches the mental model (no push, land instead of merge, what to do on a 409) and the common workflows.
- **`--json`** on every query and commit command, for scripts.

```bash
claude mcp add kudzu -e KUDZU_REMOTE=unix:///tmp/kudzu.sock -- kudzu mcp   # Claude Code; other clients: docs/AI.md
cp -r skills/kudzu ~/.claude/skills/                                        # the skill, for agents with a shell
```

More in [docs/AI.md](docs/AI.md), plus [`llms.txt`](https://garyships.github.io/kudzu/llms.txt) for LLMs.

## Numbers

These come from importing the last 529 commits (3,613 files) of a real repo, with every tier on one machine. They are illustrative, not a benchmark.

| | time |
|---|---|
| import 529 git commits, with all derived data | ~5 s |
| sparse clone of `src/` (1,838 files), cold daemon | 0.30 s |
| full clone, warm daemon | 0.46 s |
| full clone on a second machine, warm regional cache | 0.49 s |
| `git clone --no-local` of the same repo | 2.07 s |
| 50 simultaneous cold requests for the same objects | 1 upstream request |

The trade-off: with no packfiles or deltas, server storage was 79 MB, against 26 MB for git's packfile.

## Status

Kudzu is a working prototype with a test suite: `go test -race ./...`. Imports are checked against `git ls-tree` and `git blame --first-parent`. Not built yet:
- virtualized (FUSE) checkouts
- auth and per-directory ACLs
- a shared multi-server backend (Postgres or Spanner, S3)
- object GC
- export back to git

Contributions are welcome. See [AGENTS.md](AGENTS.md) for a map of the code.

## License

MIT. Kudzu is an unofficial rebuild of the system described in Anthropic's public post [Redesigning source control at Anthropic](https://www.anthropic.com/engineering/redesigning-source-control). It is not affiliated with or endorsed by Anthropic and contains no Anthropic code.
