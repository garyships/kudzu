# Kudzu for AI agents

Kudzu was built for a world where most clients are agents. You can connect one in three ways:

1. **MCP server**: `kudzu mcp` gives any MCP client (Claude Code, Claude Desktop, Cursor, …) tools for history, blame, diffs, file reads and lock-free writes. No checkout and no shell needed.
2. **Agent Skill**: [`skills/kudzu/SKILL.md`](../skills/kudzu/SKILL.md) teaches an agent that has a shell how to use the `kudzu` CLI.
3. **`--json` CLI output**: for scripts and agents that parse results.

All three talk to a Kudzu server (or a regional cache, or a node daemon) at `KUDZU_REMOTE`: `host:port`, `http(s)://host:port` or `unix:///path.sock`.

No Kudzu server yet? Run `kudzu up` inside any git repository. It imports the history and serves all three tiers, with the node daemon on `unix:///tmp/kudzu.sock`, which is the address the snippets below use. Only the user who started it can connect. If a client refuses that socket because another user owns it, it is not your daemon: run your own with `kudzu up --socket PATH` and use `unix://PATH`. Restarting it imports only new git commits and keeps what agents landed: `main` moves to the git tip only as a fast-forward, otherwise it stays put with a warning (`kudzu export` carries the landed work back to git).

## MCP server

```
kudzu mcp [--remote ADDR] [--read-only] [--author "Name <email>"]
```

| Flag | Meaning |
|---|---|
| `--remote` | Kudzu tier to talk to. Default `$KUDZU_REMOTE`, else the remote of the working copy `kudzu mcp` is started in. It exits with an error if there is none. |
| `--read-only` | Only register tools that don't write. The write tools aren't listed and can't be called. |
| `--author` | Default author for changesets the tools create. Default `$KUDZU_AUTHOR`, else `user <user@host>`. Each write tool also takes `author`. |

It speaks MCP over stdio: newline-delimited JSON-RPC 2.0 on stdin and stdout, with diagnostics on stderr. It is built on the official Go SDK (`github.com/modelcontextprotocol/go-sdk`).

### Claude Code

```bash
claude mcp add kudzu -e KUDZU_REMOTE=unix:///tmp/kudzu.sock -- kudzu mcp
# read-only, shared with your team through .mcp.json in the project:
claude mcp add -s project kudzu -e KUDZU_REMOTE=unix:///tmp/kudzu.sock -- kudzu mcp --read-only
```

Everything after `--` is the server's own command line. `-s` picks the scope: `local` (default, just you, this project), `project` (writes `.mcp.json`) or `user` (all your projects). Check the connection with `claude mcp list`, or with `/mcp` inside a session.

This repository has an example [`.mcp.json`](../.mcp.json). It runs `kudzu mcp --read-only` and takes `KUDZU_REMOTE` from your environment, falling back to `unix:///tmp/kudzu.sock`. Drop `--read-only` to let the agent write.

### Claude Desktop, Cursor and other clients

Most clients accept the same `mcpServers` JSON (Claude Desktop: `claude_desktop_config.json`, Cursor: `.cursor/mcp.json`). Desktop apps often don't inherit your shell's `PATH`, so use the absolute path from `which kudzu`:

```json
{
  "mcpServers": {
    "kudzu": {
      "command": "/usr/local/bin/kudzu",
      "args": ["mcp"],
      "env": { "KUDZU_REMOTE": "127.0.0.1:7070" }
    }
  }
}
```

Add `"--read-only"` to `args` to hide the write tools.

### Tools

This list is generated from the server's `tools/list`. `?` marks an optional argument.

| Tool | Kind | Arguments | What it does |
|---|---|---|---|
| `kudzu_resolve` | read | `rev` | Resolve a revision to its full changeset ID and root tree ID. Use it to pin a moving ref before a series of reads or a write. |
| `kudzu_log` | read | `rev`?, `path`?, `limit`? | First-parent history of a revision, or only the changesets that touched a file or directory. Default 20, at most 500. `more` says whether there are older entries. |
| `kudzu_blame` | read | `path`, `rev`?, `start_line`?, `end_line`? | Last changeset, author and date for each line, computed on the server. At most 2000 lines per call. |
| `kudzu_diff` | read | `to`?, `from`?, `merge_base`?, `path`?, `patch`?, `max_patch_bytes`? | Changed files (A/M/D) between two revisions, plus a unified diff unless `patch` is false. With only `to`, what that changeset changed. The patch is capped at 100 KB by default (1 MB max), and the result says when it was truncated. |
| `kudzu_cat` | read | `path`, `rev`?, `start_line`?, `end_line`?, `max_bytes`? | A file's content at a revision, with the changeset it was read at. Binary files are reported, not returned. Capped at 100 KB by default (1 MB max). |
| `kudzu_ls` | read | `path`?, `rev`? | A directory's entries, each with a type: dir, file, executable, symlink or submodule. |
| `kudzu_show` | read | `rev` | One changeset: author, date, message, parents, log position, root tree and changed files. |
| `kudzu_ancestry` | read | `a`, `b` | Merge base of two revisions, and whether either is an ancestor of the other (has X landed on main?). |
| `kudzu_refs` | read | `prefix`? | Refs and the changesets they point at. |
| `kudzu_stats` | read | none | Statistics of the connected tier: requests, hits, misses, upstream fetches, changesets created, lands. |
| `kudzu_wc_status` | read | `dir`? | Local changes in a working copy on the MCP server's machine (M, D, ?), plus its changeset, remote and sparse directories. |
| `kudzu_write` | write | `parent`, `message`, `files`?, `delete`?, `land`?, `author`? | Create a changeset from inline file contents (`files: [{path, content, executable?}]`) and deletions on top of `parent`, without a checkout. Uploads only blobs the server lacks. Optionally lands it on `land`. Rewritten files keep their executable bit unless `executable` is given. |
| `kudzu_land` | write | `head`, `ref`? | Land an existing changeset onto a ref (default `main`): server-side rebase plus compare-and-swap, no lock. Landing again is a no-op (`already_landed`). |
| `kudzu_rebase` | write | `head`, `onto`? | Rebase a changeset onto another revision on the server. Creates new changesets and moves no ref. |
| `kudzu_wc_commit` | write | `message`, `dir`?, `add_untracked`?, `land`?, `author`? | Commit a working copy's local changes, optionally landing them in the same call. |
| `kudzu_wc_update` | write, destructive | `dir`?, `rev`? | Move a working copy to another revision (default `main`). Refuses if tracked files are modified. Untracked files at paths the new revision adds are overwritten. |

Read tools are annotated `readOnlyHint: true`. Write tools carry `destructiveHint: false`, except `kudzu_wc_update`, which is `true`. All tools set `openWorldHint: false`. `--read-only` keeps the eleven read tools.

**Results.** Each result is a JSON summary as text. `kudzu_cat`, `kudzu_blame` and `kudzu_diff` add the file content, the blame lines or the patch as a second text block, so it isn't JSON-escaped. Revisions in results are full 64-character changeset IDs.

**Errors** are tool results with `isError: true`, never protocol errors. That covers bad arguments, unknown revisions or paths, an unreachable server and conflicts. A conflict names the conflicting paths and says what to do next, for example:

```
conflict: created changeset 3f2a… but could not land it on main. Conflicting paths: services/api/handler.go.
Someone else landed overlapping changes to these files. Re-read them at main, redo your edit on top of them,
and call kudzu_write again with parent set to the changeset kudzu_cat reports
```

**Writing safely.** Read with `kudzu_cat`, which reports the `changeset` it read. Then pass that ID as `parent` to `kudzu_write` with `land: "main"`. The server merges anything that landed in between, line by line, and fails with a conflict only when both sides changed the same or adjacent lines. If you pass `parent: "main"` instead, the file contents you send replace whatever main has now.

## Agent Skill

[`skills/kudzu/SKILL.md`](../skills/kudzu/SKILL.md) is an Agent Skill. It covers the mental model compared with git, a command cheat sheet, the common workflows (history without a checkout, sparse clone → commit `--land`, recovering from a conflict, API-only writes), `--json`, and the MCP tools. Install it where your agent looks for skills:

```bash
# Claude Code, for one project (commit it to share with your team)
mkdir -p .claude/skills && cp -r /path/to/kudzu/skills/kudzu .claude/skills/
# Claude Code, for all your projects
mkdir -p ~/.claude/skills && cp -r /path/to/kudzu/skills/kudzu ~/.claude/skills/
# or straight from GitHub
curl -fsSL --create-dirs -o .claude/skills/kudzu/SKILL.md \
  https://raw.githubusercontent.com/garyships/kudzu/main/skills/kudzu/SKILL.md
```

Other agents that support skills use the same `kudzu/SKILL.md` folder in their own skills directory. Agents without skill support can be given the file as context.

## `--json` CLI output

These commands take `--json` and print exactly one JSON document on stdout:

| Command | Output |
|---|---|
| `log`, `blame`, `diff`, `show`, `ls`, `refs`, `merge-base` | The server's HTTP API response, unchanged (shapes in [llms-full.txt](llms-full.txt)) |
| `diff` in a working copy, without `--from`/`--to` | `{"files": [{"path", "status"}], "patch"}`, with uncommitted changes to tracked files only |
| `status` | `{"changeset", "files": [{"path", "status"}]}`, where status is `M`, `D` or `?` |
| `commit` | `{"id", "tree", "changes", "land"?: {"ref", "landed", "previous", "attempts", "rebased"}}` |
| `land` | `{"ref", "landed", "previous", "attempts", "rebased"}` |
| `write` | `{"id", "tree", "seq", "files", "uploaded", "land"?}` |

On failure, the exit code is 1 and stderr gets `{"error": "...", "status": 409, "conflicts": ["path", ...]}`. `status` and `conflicts` appear only when the server returned them.

```bash
kudzu log -n 5 --json | jq -r '.[] | "\(.id[:12]) \(.message)"'
kudzu commit -m "fix" --land main --json 2>err.json || jq -r '.conflicts[]?' err.json
```

## More

- [llms.txt](llms.txt) is an index for LLMs. [llms-full.txt](llms-full.txt) is the full CLI, MCP and HTTP API reference in one file.
- [DESIGN.md](DESIGN.md) explains how Kudzu works inside.
- [AGENTS.md](../AGENTS.md) is for agents working on Kudzu's own code.
