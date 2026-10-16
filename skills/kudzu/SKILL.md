---
name: kudzu
description: Use when working with source code kept in Kudzu, a server-side version control system for monorepos, instead of git. Signs are a `.kudzu/` directory in the working copy, `KUDZU_REMOTE` in the environment, the `kudzu` CLI, or `kudzu_*` MCP tools, or the user mentioning Kudzu. Covers reading history, blame and diffs without a checkout, sparse clones, committing and landing (`kudzu commit --land main`), recovering from land conflicts (409), writing files through the API without a checkout, and `--json` output for scripts.
---

# Using Kudzu

Kudzu keeps history on the server. A working copy holds the files of one revision (often a sparse subset), and every question about history goes to the server.

## Mental model (if you know git)

- **No clone of history.** `.kudzu/` holds no history. `log`, `blame`, `diff`, `show`, `cat` and `ls` are server queries, so they work from any directory as long as a remote is set. No checkout needed.
- **No push.** `kudzu commit` creates the changeset on the server right away. It exists there whether or not it lands.
- **Land, not merge.** `--land main` (or `kudzu land`) asks the server to rebase your changeset onto main's current tip and move main with compare-and-swap. No merge queue, no lock. Concurrent lands that touch different files, or separate parts of a file, all succeed. Edits to the same or adjacent lines fail with a **conflict that lists the paths**.
- **No local branches.** A changeset is addressed by its ID. Refs like `main` are names on the server.
- **Revisions:** a ref (`main`), a changeset ID or a unique hex prefix (4+ chars), plus git-style suffixes: `~N` for the Nth first-parent ancestor (`main~3`) and `^N` for the Nth parent (`main^2` is a merge's second parent), chained left to right (`main~2^2~1`). Inside a working copy, `-r` defaults to the checked-out changeset. Elsewhere it defaults to `main`.
- **Remote:** `--remote ADDR` > `$KUDZU_REMOTE` > the working copy's recorded remote. ADDR is `host:port`, `http(s)://host:port` or `unix:///path.sock` (a node daemon).
- **Author:** `--author "Name <email>"` > `$KUDZU_AUTHOR` > `user <user@host>`.
- **Token:** if the server enforces access control, set `$KUDZU_TOKEN`. A 403 names the path or ref you may not read, write or land; don't retry it, work within the paths you are allowed or ask for access.
- Changesets record changes relative to their first parent. History and blame follow first parents.

No server to talk to? `kudzu up` inside any git repository imports its history and serves everything on `unix:///tmp/kudzu.sock` until Ctrl-C. Restarting it keeps what you landed on `main`: it warns instead of moving `main` back to the git tip. If a command refuses that socket because another user owns it, start your own with `kudzu up --socket PATH` and set `KUDZU_REMOTE=unix://PATH`.

## Cheat sheet

| Task | Command |
|---|---|
| History of the repo / of a path | `kudzu log [-r REV] [-n 20] [--oneline] [PATH]` |
| Who changed each line | `kudzu blame [-r REV] PATH` |
| What a changeset changed | `kudzu show [REV]` · `kudzu diff --to REV` |
| Diff two revisions | `kudzu diff --from A --to B [--stat] [PATH]` |
| What a branch adds since it forked | `kudzu diff --merge-base --from main --to X` |
| Read a file / list a directory | `kudzu cat [-r REV] PATH` · `kudzu ls [-r REV] [DIR]` |
| Refs, merge base | `kudzu refs` · `kudzu merge-base A B` |
| Sparse clone | `kudzu clone [-r REV] [--sparse dir1,dir2] REMOTE DIR` |
| Local changes | `kudzu status` (M modified, D deleted, ? untracked) · `kudzu diff` (no `--from`/`--to`) |
| Commit (and land) | `kudzu commit -m MSG [-A] [--land main]` (`-A` adds untracked files) |
| Land / rebase what you committed | `kudzu land [--ref main]` · `kudzu rebase [-d main]` |
| Move the checkout | `kudzu update [-r REV]` (refuses if tracked files are modified) |
| Widen or narrow the checkout | `kudzu sparse [show \| set DIRS \| add DIRS \| remove DIRS]` |
| Commit without any checkout | `kudzu write -r PARENT -m MSG [--land main] [--delete PATH]... PATH=@FILE \| PATH=@-` |

Go-style flags: write `-n 5` (not `-n5`). Flags and arguments can come in any order.

## Workflows

### Inspect history without a checkout

```bash
export KUDZU_REMOTE=unix:///tmp/kudzu.sock   # or host:port
kudzu log -n 10 --oneline
kudzu log --oneline services/api              # only changesets that touched this path
kudzu blame services/api/handler.go
kudzu diff --from main~5 --to main --stat
kudzu cat -r main~5 services/api/handler.go
```

### Sparse clone → edit → commit and land

```bash
kudzu clone --sparse services/api "$KUDZU_REMOTE" ~/wc && cd ~/wc
# edit files under services/api (files directly in parent dirs, like README.md, are included too)
kudzu status
kudzu diff                                   # your uncommitted changes
kudzu commit -m "api: fix timeout" --land main
```

If the land had to rebase, your checkout moves to the landed changeset automatically. Run `kudzu sparse add services/web` to see more of the tree, and `kudzu update` to catch up with main.

### A land fails with a conflict (409)

`kudzu commit ... --land main` reports `conflict in <paths>`. Someone landed overlapping edits first. Nothing changed locally: your edits are still uncommitted. Recover like this:

```bash
kudzu commit -m "api: fix timeout"               # keep your change as changeset X (no --land); note its id
kudzu update -r main                             # clean checkout of the new main
kudzu diff --to X | patch -p1                    # re-apply; hunks that clash go to *.rej
# merge each *.rej by hand, then delete the *.rej and *.orig files
kudzu commit -m "api: fix timeout" --land main
```

`kudzu commit --json` prints X's `id`. `kudzu land` and `kudzu rebase` hit the same conflict, because the server replays X itself. You have to redo the change on top of the new main.

### Write through the API with no checkout

```bash
REV=$(kudzu log -n 1 --json | jq -r '.[0].id')   # pin the revision you read
kudzu cat -r "$REV" config/app.yaml > app.yaml
# ... edit app.yaml ...
kudzu write -r "$REV" -m "config: raise limit" --land main config/app.yaml=@app.yaml
printf 'v2\n' | kudzu write -r "$REV" -m "bump" --land main VERSION=@-
kudzu write -r main -m "drop old file" --land main --delete old/file.txt
```

Use as parent (`-r`) the changeset you **read** the file at, not `main` at write time. Then edits that landed in between are merged, not overwritten. A conflict is reported as above, and nothing lands.

## Machine-readable output (`--json`)

`log`, `blame`, `diff`, `show`, `ls`, `refs`, `merge-base`, `status`, `commit`, `land` and `write` accept `--json`. Each prints one JSON document on stdout. Query commands print the server's HTTP API response as it is. On failure, stderr gets `{"error": ..., "status": 409, "conflicts": [...]}` and the exit code is 1.

```bash
kudzu status --json        # {"changeset": "...", "files": [{"path": "a.go", "status": "M"}]}
kudzu commit -m msg --land main --json   # {"id", "tree", "changes", "land": {"ref", "landed", "previous", "attempts", "rebased"}}
kudzu diff --to main --stat --json | jq -r '.files[].path'
```

## MCP alternative

If you have `kudzu_*` tools (the `kudzu mcp` server), use them instead of the CLI. They need no shell and no checkout:
`kudzu_log`, `kudzu_blame`, `kudzu_diff`, `kudzu_cat`, `kudzu_ls`, `kudzu_show`, `kudzu_resolve`, `kudzu_ancestry`, `kudzu_refs`, `kudzu_stats` to read.
`kudzu_write` (files in, changeset out, optional `land`), `kudzu_land` and `kudzu_rebase` to write.
`kudzu_wc_status`, `kudzu_wc_commit`, `kudzu_wc_update` for a checkout on the MCP server's machine.
`kudzu_cat` returns the changeset it read. Pass that ID as `parent` to `kudzu_write`.
Setup: `claude mcp add kudzu -e KUDZU_REMOTE=unix:///tmp/kudzu.sock -- kudzu mcp`. See `docs/AI.md` in the Kudzu repository.
