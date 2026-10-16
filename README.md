<div align="center">

<img src="docs/assets/logo/kudzu-icon.svg" width="88" alt="Kudzu logo: a commit graph that grows a leaf">

# Kudzu

### Git was built for humans. Kudzu is built for agents.

**Version control infrastructure for the agent era.**<br/>
No full clones. History as an API. Lock-free landing for thousands of concurrent agents. MCP-native.

[![release](https://img.shields.io/github/v/release/garyships/kudzu?color=3fb950)](https://github.com/garyships/kudzu/releases)
[![ci](https://github.com/garyships/kudzu/actions/workflows/ci.yml/badge.svg)](https://github.com/garyships/kudzu/actions/workflows/ci.yml)
[![MCP: 16 tools](https://img.shields.io/badge/MCP-16_tools-8b5cf6)](docs/AI.md)
[![license: MIT](https://img.shields.io/badge/license-MIT-3fb950)](LICENSE)
![go 1.24+](https://img.shields.io/badge/go-1.24%2B-00ADD8)

[**Website**](https://garyships.github.io/kudzu/) · [**Demo video**](https://garyships.github.io/kudzu/#demo) · [Design](docs/DESIGN.md) · [For AI agents](docs/AI.md) · [中文](README.zh-CN.md)

<img src="docs/assets/demo.gif" alt="Kudzu demo: import git history, sparse clone, concurrent lock-free lands" width="860">

<sub>⭐ Agents are about to outnumber the engineers who run them. If you think source control should be ready, star the repo.</sub>

</div>

## Agents break git

Every engineer is about to run a hundred agents. They clone, commit and merge around the clock, all at once. Git was designed for one person on a laptop, and it breaks first:

| | What breaks | What Kudzu does |
|---|---|---|
| 🌪️ **Clone storm** | Every agent sandbox and CI job pulls the entire repo and all of its history. | Clients fetch only the objects they touch, through tiered caches. 50 identical cold requests become 1 upstream request. |
| 🚦 **Merge jam** | Thousands of agents race for `main`. Merge queues take a lock, pushes bounce, and everyone rebases and retries. | The server rebases and lands with a compare-and-swap on the ref. No lock, no queue, no push. |
| 🔒 **History trapped in clones** | To ask "who changed this line?", an agent has to check out the repo first. | `log`, `blame` and `diff` are server APIs and [16 MCP tools](#-version-control-your-agent-can-drive). |
| 🪞 **Mirror sprawl** | Hundreds of mirrors, each a full copy, each needing repack and gc. | Immutable, content-addressed objects behind cache tiers. No packfiles, no maintenance. |

**This isn't hypothetical.** Anthropic's monorepo went from ~2,500 to 30,000+ merges a day in four months, and git couldn't keep up. They replaced it with an internal system, Scion, and [published the design](https://www.anthropic.com/engineering/redesigning-source-control). Kudzu builds that architecture in the open, for everyone. The first version shipped the day after the post.

## Try it in 60 seconds

```bash
curl -fsSL https://raw.githubusercontent.com/garyships/kudzu/main/install.sh | sh
cd any-git-repo && kudzu up      # imports the history, runs server → cache → daemon
```

Then, in another terminal:

```bash
export KUDZU_REMOTE=unix:///tmp/kudzu.sock
kudzu log -n 5 --oneline                          # history without a checkout
kudzu blame README.md                             # computed on the server
kudzu clone --sparse src $KUDZU_REMOTE /tmp/wc    # fetches only src/
```

With Go: `go install github.com/garyships/kudzu/cmd/kudzu@latest`. Windows builds are on the [releases page](https://github.com/garyships/kudzu/releases).

## 🤖 Version control your agent can drive

Kudzu speaks MCP. One line connects Claude Code, and any other MCP client works the same way:

```bash
claude mcp add kudzu -e KUDZU_REMOTE=unix:///tmp/kudzu.sock -- kudzu mcp
```

Then just ask:

> *"Who last changed the retry logic in `services/api/handler.go`, and what else did that changeset touch?"*<br/>
> *"What landed under `services/api` today?"*<br/>
> *"Bump the version in `VERSION` and land it on main."* No checkout and no push: the server rebases and lands it.

| 11 read tools | 5 write tools |
|---|---|
| `kudzu_log` `kudzu_blame` `kudzu_diff` `kudzu_cat` `kudzu_ls` `kudzu_show` `kudzu_resolve` `kudzu_ancestry` `kudzu_refs` `kudzu_stats` `kudzu_wc_status` | `kudzu_write` (files in, landed changeset out) `kudzu_land` `kudzu_rebase` `kudzu_wc_commit` `kudzu_wc_update` |

- `--read-only` hides the write tools.
- Agents with a shell can use the [Agent Skill](skills/kudzu/SKILL.md) instead.
- Every query and commit command takes `--json`.

Setup for Cursor, Claude Desktop and other clients: [docs/AI.md](docs/AI.md). There is also [`llms.txt`](https://garyships.github.io/kudzu/llms.txt) for LLMs.

## Kudzu vs git

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

## Run the tiers separately

`kudzu up` runs all three tiers in one process. In production each tier runs on its own: one server, a cache per region, a daemon per machine.

<details>
<summary>Server, regional cache and node daemon as separate processes</summary>

```bash
# import an existing git repo (re-run any time; only new commits are ingested)
kudzu import --store ./data --git /path/to/repo --as main
# ...and keep a git mirror of what lands in Kudzu (also incremental)
kudzu export --store ./data --git /path/to/mirror.git --ref main

kudzu server --store ./data --listen 127.0.0.1:7070 &
# or several stateless servers sharing Postgres and an S3-compatible bucket:
#   kudzu server --meta postgres://... --objects s3://bucket/kudzu --listen ...
kudzu cache  --upstream 127.0.0.1:7070 --listen 127.0.0.1:7071 &
kudzu daemon --upstream 127.0.0.1:7071 --listen unix:///tmp/kudzu.sock &
export KUDZU_REMOTE=unix:///tmp/kudzu.sock

# sparse clone, edit, land
kudzu clone --sparse services/api $KUDZU_REMOTE wc && cd wc
echo "// hi" >> services/api/handler.go
kudzu commit -m "api: say hi" --land main

# or skip the checkout entirely
kudzu write -r main -m "bump version" --land main VERSION=@./VERSION
```

`scripts/demo.sh /path/to/git/repo` runs the whole flow end to end and prints each tier's cache statistics.

</details>

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

Kudzu is a working prototype with a test suite: `go test -race ./...`. Imports are checked against `git ls-tree` and `git blame --first-parent`, and exports round-trip back to git. Also built: per-directory access control (`kudzu server --auth`), several stateless servers on Postgres and S3-compatible storage (`--meta`, `--objects`), garbage collection (`kudzu gc --store`) and lazily fetched FUSE checkouts that you can edit and commit from (`kudzu mount --writable`, built with `-tags fuse`). Not built yet:
- Spanner or CockroachDB metadata stores (Postgres works today)

Contributions are welcome. Pick up a [help-wanted issue](https://github.com/garyships/kudzu/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22), and see [AGENTS.md](AGENTS.md) for a map of the code.

## License

MIT. Independent open-source project. Architecture based on Anthropic's public write-up of Scion, [Redesigning source control at Anthropic](https://www.anthropic.com/engineering/redesigning-source-control).

---

<div align="center">

[![Star History Chart](https://api.star-history.com/svg?repos=garyships/kudzu&type=Date)](https://star-history.com/#garyships/kudzu&Date)

</div>
