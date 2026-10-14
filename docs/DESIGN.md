# Kudzu design

How Kudzu implements each idea from Anthropic's post [Redesigning source control at Anthropic](https://www.anthropic.com/engineering/redesigning-source-control), and which choices the post left open. Paths are relative to the repository root. [中文版](DESIGN.zh-CN.md)

## 1. Data model

### Changesets (`internal/object/changeset.go`)

The unit of work is a **changeset**, an entry in an append-only log. Unlike a git commit it does not point at a root tree. It lists the files that changed relative to its **first parent**:

```json
{
  "parents": ["<id>"],
  "author": "Alice <a@example.com>", "time": 1760000000, "tz": 480,
  "message": "api: tweak handler",
  "changes": [
    {"path": "services/api/handler.go", "mode": 33188, "blob": "<sha256>"},
    {"path": "services/api/old.go", "mode": 0}
  ]
}
```

`mode: 0` means deleted. The ID is the SHA-256 of the canonical JSON. Merges also list changes relative to the first parent, matching the post's footnote: in trunk-based monorepos people care about mainline history and blame.

### Objects (`internal/object/object.go`, `tree.go`)

- **blob**: file contents.
- **tree**: a directory snapshot in a git-like encoding (`<octal mode> <name>\0<32-byte id>`). Identical directories hash identically across all changesets, so a client never asks twice for a subtree it already has.
- **unode**: a history-aware node (Mononoke's term). It records the changeset that produced it (its linknode) and the previous version at the same path.

Object IDs are `sha256("<kind> <len>\0" + content)`. The **stored form** `[kind][codec][payload]` is identical everywhere: on the server's disk, in the regional cache's memory, on a node daemon's disk, and on the wire. As the post says, a cache streams exactly the bytes the daemon will store, without computing deltas. Only blobs are compressed; trees and unodes are mostly hashes and don't compress.

Tree decoding rejects names like `..` or `a/b`, so a hostile server cannot make a client write outside its working copy.

## 2. Storage (`internal/store`)

The post describes two storage roles. Both are interfaces in Kudzu:

| Role | Interface | Implementation today | Production candidates |
|---|---|---|---|
| Authoritative metadata: changeset log, refs, derived data | `store.Meta` | SQLite in WAL mode (`meta.go`) | Spanner, Postgres, CockroachDB |
| Immutable objects | `store.Objects` | directory (`FS`), memory LRU (`Mem`), layered (`Cached`) | S3, GCS |

SQLite tables:

- `changesets(seq, id, data)`: the append-only log. `seq` is the log position; `/api/wal` lets indexers follow it.
- `refs(name, target, version)`: moving a ref is a **compare-and-swap on one row**, separate from creating changesets.
- `derived(kind, key, value)`: a cache of derived data that any server can fill in.
- `mapping(ns, key, value)`: git sha → changeset, for incremental imports.

Creating a changeset is one `INSERT OR IGNORE`: no locks, no cross-server coordination, no refs touched.

## 3. The derived-data engine (`internal/repo/derive.go`)

A changeset only says what changed. It can't answer "who last touched this line?" Everything that isn't part of the changeset itself is **derived data**, computed by **one generic mechanism**. Each type is defined recursively: its value for a key is computed from its values for the key's parents.

```go
type Deriver struct {
    Name    string
    Parents func(ctx, r, key) ([]object.ID, error)
    Compute func(ctx, r, key, parentValues [][]byte) ([]byte, error)
}
```

`Repo.Derive` does the rest. It checks caches, then computes missing ancestors oldest-first. It walks iteratively, so long histories don't overflow the stack. It writes results to `derived` and collapses concurrent computations of the same key.

| Name | Key | Value | Used for | Default |
|---|---|---|---|---|
| `tree` | changeset | root tree ID | checkout, diff, reading files | computed at creation (also validates the changeset) |
| `gen` | changeset | generation number + first-parent skip list (`skip[k]` = ancestor 2^k steps back) | O(log n) ancestry checks and `main~N` | eager |
| `unode` | changeset | root unode ID | history of a file or directory | eager (`--lazy unode` makes it lazy) |
| `blame` | file unode | per-line attribution | blame | lazy (computed on first query) |

The post's three benefits hold:

1. Adding an index means adding a `Deriver`.
2. Each type can be computed eagerly or lazily (`Repo.Eager`).
3. Clients never see these structures, so their internals can change freely.

**Fast path history.** Every changeset that touches a file or directory creates a new unode for it, pointing to the previous unode at that path. `kudzu log <path>` walks that chain. It reads one node per result, however much unrelated history lies in between.

**Blame** uses the file unode's previous version as its parent. Unchanged lines keep their attribution; new or changed lines go to the current linknode. Diffs use Myers' algorithm (`internal/textdiff`).

## 4. Server-side writes (`internal/repo/write.go`)

- **Create** validates paths and blobs, drops no-op changes, and applies the changes to the parent's tree. Only directories on the path to a changed file are rewritten, so the cost is about (changed files × depth). It then appends to the log and computes the eager derived types.
- **threeWay** compares base and ours for each changed path. Paths untouched upstream apply as-is. Text files changed on both sides get a line-level three-way merge. Anything else is a conflict and returns the conflicting paths.
- **Rebase** moves the changesets in `merge-base(head, onto)..head` onto `onto`, one by one, on the server.
- **Merge** creates a two-parent changeset whose changes are listed relative to ours.
- **Land** is an optimistic loop with no global lock:

```
loop:
    tip = read ref
    head already in tip?                  -> done
    next = head descends from tip ? head : rebase(head, tip)
    CAS(ref, tip, next) succeeds          -> done
    otherwise someone landed first        -> retry
```

This follows the post's conclusion that a merge queue's global lock doesn't fit agent scale. Non-conflicting concurrent changes rebase in one after another; in the tests, 40 concurrent lands all succeed and main stays linear. Conflicts go back to the caller, usually an agent. The post moves testing to after merge and before deploy; testing is outside Kudzu's scope.

## 5. Ancestry (`internal/repo/ancestry.go`)

- **`IsAncestor(a, b)`** first binary-lifts along b's first-parent chain with the skip list, to the oldest changeset whose generation is still ≥ a's, and checks whether that is a. On trunk-based history this takes O(log n). If not, it falls back to a full search pruned by generation, which covers ancestry through a merge's second parent.
- **`MergeBase(a, b)`** paints from both sides in decreasing generation order. The first node reached from both sides is a best common ancestor.
- **`NthAncestor`** jumps on the bits of n, lowest bit first. A changeset at depth D has every skip level with 2^k ≤ D, so the needed level always exists when n ≤ D.

## 6. Object protocol (`internal/proto`)

```
GET  /objects/{id}      one object in stored form, Cache-Control: immutable
POST /objects/fetch     body: concatenated 32-byte IDs
                        response: a stream of frames  id(32) | status(1) | length(4) | data
```

**Negotiation is want-based.** The client never says what it has; it asks for specific objects by ID. Starting from the root tree, it requests each level's missing trees in one batch, expands them, and repeats. Then it requests blobs in batches. As a result:

- Every request is for an immutable object, so any cache can answer it.
- There is no multi-gigabyte packfile. Responses stream object by object, and an interrupted transfer re-asks only for what didn't arrive (`Remote.fetchChunk`).
- Sparse checkouts need no protocol support: the client just doesn't ask for subtrees it doesn't want.

Uploads go the other way: `POST /objects/missing` asks which blobs the server lacks, and `POST /objects/upload` sends only those. The server verifies every hash.

## 7. Cache tiers (`internal/tier`)

`kudzu cache` (memory) and `kudzu daemon` (disk, unix socket) are the same code with a different local store:

- Hits are served immediately; misses are fetched upstream in batches.
- **Concurrent misses coalesce.** A request for an object already being fetched waits for that fetch. In the tests, 50 simultaneous cold requests produce one upstream request. Fetches run in a detached context, so a client hanging up doesn't fail the others waiting.
- **Upstream concurrency is capped** (`--upstream-concurrency`). This is the post's failure-domain isolation: a struggling regional cache can't take down the servers above it.
- Everything that isn't an object request (`/api/*`, uploads) is proxied upstream, so all tiers look the same to a client.
- A daemon can start from a snapshot (`--seed`, produced by `kudzu snapshot`) and then fetch only what changed, as the post's daemons start from a periodically refreshed snapshot.
- `/stats` reports hit rate, coalesced misses and upstream traffic.

## 8. Client (`internal/client`)

- **Sparse rules** (`internal/sparse`, same as git's cone mode): everything under a chosen directory, plus the files directly inside each of its ancestors.
- **Clone** fetches trees level by level, then blobs in batches. Blobs coming off the stream are verified and written straight into the working copy on a worker pool; nothing is written to disk and read back.
- **Local cache**: trees are always cached in `.kudzu/objects`. Blobs are cached only when the remote is not a local daemon, since the daemon already caches them for every job on the machine (`State.CacheBlobs`). The cache is size-bounded and evicts least recently used objects.
- **Update** walks the old and new trees together, skipping subtrees with equal hashes, so its cost tracks the size of the difference.
- **Status** trusts files whose size and mtime match the index, and hashes the rest.
- **Commit** asks the server which blobs it lacks, uploads only those, and asks the server to create the changeset. There is no push: the changeset exists on the server as soon as the call returns.

## 9. Git import (`internal/gitimport`)

The post's migration used a one-way git-to-Scion ingestion pipeline; Kudzu's importer works the same way:

- `git rev-list --topo-order --reverse --parents` gives commit order.
- A single `git cat-file --batch` process reads commits, trees and blobs.
- Kudzu diffs the two git trees itself, skipping equal subtrees, to get changes relative to the first parent.
- New blobs are hashed, compressed and stored on a worker pool.
- The `mapping` table records git sha → changeset, so re-running the import only ingests new commits. That makes it usable for continuously mirroring a git repository.
- Shallow-clone boundary commits start history.

Tests check that every imported commit's tree (modes, symlinks, file/directory swaps, merges) matches `git ls-tree`, and that first-parent blame matches `git blame --first-parent` line by line.

## 10. From the post vs Kudzu's own choices

**Stated in the post:**
- Changesets as log entries with changes relative to the first parent; content-addressed blobs.
- Refs as a single-row compare-and-swap.
- Recursive derived data through one mechanism, eager or lazy per type.
- Three tiers answering identical RPCs, with strict tiering.
- Want-based negotiation from the root tree; no packfiles.
- Server-side merge and rebase.
- Sparse and virtualized checkouts.
- Testing after merge.

**Kudzu's own choices (Scion may differ):**
- Serialization formats, SHA-256, zlib.
- Unodes for path history; the post only mentions dedicated structures.
- The exact skip-list definition: binary lifting on the first-parent chain.
- Conservative three-way merge: adjacent edits conflict.
- HTTP/1.1 with a custom frame format instead of gRPC. There is no codegen, and `curl` works.
- SQLite instead of Spanner, so there is one authoritative server process today. There can be any number of cache tiers.

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/resolve?rev=` | revision → changeset + root tree (a ref or ID prefix, optionally with `~N` / `^N` suffixes, e.g. `main~2^2`) |
| GET | `/api/changesets/{rev}` | changeset details |
| POST | `/api/changesets` | create a changeset; optional `"land": "main"` lands it in the same call |
| GET | `/api/log?rev=&path=&limit=` | first-parent history, or a path's history |
| GET | `/api/blame?rev=&path=` | per-line changeset, author, time |
| GET | `/api/diff?from=&to=&path=&merge_base=1&patch=1` | changed files, optionally with a unified diff |
| GET | `/api/cat?rev=&path=`, `/api/ls?rev=&path=` | read a file, list a directory |
| GET | `/api/ancestry?a=&b=` | merge base and ancestry |
| GET / POST | `/api/refs`, `/api/refs/{name}` | list refs; compare-and-swap a ref |
| POST | `/api/rebase`, `/api/merge`, `/api/land` | server-side rebase, merge, lock-free land |
| GET | `/api/wal?since=` | follow new changesets by log position (e.g. for a search indexer) |
| GET | `/api/snapshot?rev=&sparse=` | all objects of a revision (for seeding daemons) |
| GET / POST | `/objects/{id}`, `/objects/fetch` | object protocol, served by every tier |
| POST | `/objects/missing`, `/objects/upload` | which objects the server lacks; upload |
| GET | `/stats`, `/healthz` | statistics, health |
