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

Tree decoding rejects names like `..` or `a/b`, so a hostile server cannot make a client write outside its working copy. No path may go through `.git` or `.kudzu` at any depth, ignoring case and trailing dots and spaces (`.GIT`, `.git.`), nor through their NTFS short names `git~1` and `kudzu~1`: a nested `.git/config` makes every git command run in that directory execute whatever its `core.fsmonitor` says. Create refuses to add such a path (it may still delete one), and a checkout refuses to write one.

## 2. Storage (`internal/store`)

The post describes two storage roles. Both are interfaces in Kudzu:

| Role | Interface | Implementation today | Production candidates |
|---|---|---|---|
| Authoritative metadata: changeset log, refs, derived data | `store.Meta` | SQLite in WAL mode (`meta.go`); Postgres (`postgres.go`) | Spanner, CockroachDB |
| Immutable objects | `store.Objects` | directory (`FS`), memory LRU (`Mem`), layered (`Cached`); S3-compatible buckets (`s3.go`: S3, GCS, R2, MinIO) | |

SQLite tables:

- `changesets(seq, id, data, created)`: the append-only log. `seq` is the log position; `/api/wal` lets indexers follow it. `created` is when the entry was last appended, for gc's grace period.
- `refs(name, target, version)`: moving a ref is a **compare-and-swap on one row**, separate from creating changesets.
- `derived(kind, key, value)`: a cache of derived data that any server can fill in.
- `mapping(ns, key, value)`: git sha → changeset (`git`) and changeset → git sha (`git-export`), for incremental imports and exports.

Creating a changeset is one insert (an upsert that refreshes `created` if the changeset already exists): no locks, no cross-server coordination, no refs touched.

### Several servers (`--meta`, `--objects`)

With the local defaults there is one authoritative server process. `kudzu server --meta postgres://… --objects s3://bucket/prefix` keeps both roles in shared services instead, so any number of stateless servers can run behind a load balancer, as the post intends:

- **Postgres** holds the same four tables. A ref moves by `UPDATE refs … WHERE name = $1 AND target = $old` and checks the rows affected, so lands on different servers race exactly as they do in one process: the loser rebases and retries. Derived data is `INSERT … ON CONFLICT DO NOTHING`, since every server computes the same value. The one subtlety is the log: a sequence hands out `seq` at insert time, not commit time, so two concurrent appends could commit out of order and a `/api/wal` reader could skip one. Appends therefore take a transaction-scoped advisory lock around a single-row insert, which keeps commit order equal to log order.
- **S3-compatible buckets** hold objects under `prefix/ab/cdef…`. Objects are immutable, so servers never coordinate over them, and each server's memory cache can keep any object indefinitely. Kudzu signs requests itself (AWS Signature Version 4, checked against AWS's published examples), so there is no cloud SDK dependency. `gs://` uses GCS's S3-compatible XML API with HMAC keys; `?endpoint=` points at MinIO, R2 or anything else S3-compatible. gc's touch is a server-side copy of an object onto itself, which refreshes `Last-Modified` without moving data.

The repo and end-to-end tests run against these backends too when `KUDZU_TEST_POSTGRES` and `KUDZU_TEST_S3` are set (`internal/store/storetest`); CI does this with Postgres and MinIO.

### Garbage collection (`internal/repo/gc.go`)

A land that loses its compare-and-swap race, a rebase that hits a conflict, and a commit that is never landed all leave changesets and objects that no ref reaches. `kudzu gc --store DIR --keep 30d` reclaims them:

1. **Mark.** Every changeset reachable from a ref, or appended within the grace period (`--keep`), is live, and so are its ancestors. A live changeset's root tree and everything below it are live, and so is its root unode with its children and previous versions. Subtrees already marked are skipped, so marking costs about the number of distinct trees, not changesets × files.
2. **Sweep.** Unreachable changesets older than the grace period are deleted, with their derived data (`tree`, `gen`, `unode`; `blame` for their unodes) and any `mapping` row naming them. Then every object that is neither marked nor written or touched within the grace period is deleted. Leftover temporary files go too.

The log stays append-only in the sense that matters: positions never change. Removed entries leave gaps in `seq`, which `/api/wal` readers skip without noticing, so a reader's `since` cursor stays valid. Tombstones would keep growing the log for no reader's benefit.

gc can run while the server is up. The grace period carries that: the server *touches* (bumps the mtime of) every object it tells a client it already has (`/objects/missing`) and every existing object a new changeset reuses, re-appending a changeset refreshes its `created`, and the sweep re-checks each candidate's age right before deleting it. Create checks parents against the log, not a cache, so nothing can be built on a changeset gc removed. A grace period well above the server's touch interval (10 minutes) is required for this; use `--keep 0s` only on an idle store. `--dry-run` reports what would go.

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
- **A checkout trusts no tree.** Besides the checks in §1, it refuses names the client's system would not store as they are: on Windows, names with `\ / : * ? " < > |` or control characters, a trailing dot or space, or a device name such as `CON` or `aux.c`; on macOS and Windows, two names in one directory that differ only in case.
- **Local cache**: trees are always cached in `.kudzu/objects`. Blobs are cached only when the remote is not a local daemon, since the daemon already caches them for every job on the machine (`State.CacheBlobs`). The cache is size-bounded and evicts least recently used objects.
- **Update** walks the old and new trees together, skipping subtrees with equal hashes, so its cost tracks the size of the difference.
- **Status** trusts files whose size and mtime match the index, and hashes the rest. A nested git checkout's `.git` is ignored, since no changeset may contain it.
- **Commit** asks the server which blobs it lacks, uploads only those, and asks the server to create the changeset. There is no push: the changeset exists on the server as soon as the call returns.

### Virtualized checkout (`internal/vfs`, `internal/fusefs`)

`kudzu mount [-r REV] [--sparse dirs] REMOTE MOUNTPOINT` presents a revision as a read-only filesystem that downloads nothing up front, so a job pays only for what it reads:

- A directory's tree object is fetched the first time the directory is listed or a path through it is looked up; a file's blob, the first time the file is read. Both go through the object protocol, normally to the node daemon, are verified like any other object, and are kept in a local cache (memory, or `--cache DIR`). Concurrent reads of the same object share one fetch.
- Trees don't record file sizes, and `stat` must report them before a file is read. `GET /api/sizes?tree=ID` returns them for one directory in one request (the server reads the blobs it already has), so `ls -l` is exact without moving any file content.
- A revision never changes, so the kernel may cache entries, attributes and pages indefinitely (`FOPEN_KEEP_CACHE`, long timeouts). Files take their changeset's time as mtime. Sparse cones and access-control filtering apply as in a checkout.
- `internal/vfs` has no FUSE dependency and is tested on its own; `internal/fusefs` adapts it with [go-fuse](https://github.com/hanwen/go-fuse) and is only compiled with `-tags fuse`, so the default binary does not include it. Writes (an overlay that `kudzu commit` reads) are future work.

The test mounts a revision and runs `ls`, `ls -l`, `cat` and `grep -r`, checking after each that only the objects actually read were fetched.

## 9. Git import (`internal/gitimport`)

The post's migration used a one-way git-to-Scion ingestion pipeline; Kudzu's importer works the same way:

- `git rev-list --topo-order --reverse --parents` gives commit order.
- A single `git cat-file --batch` process reads commits, trees and blobs.
- Kudzu diffs the two git trees itself, skipping equal subtrees, to get changes relative to the first parent.
- New blobs are hashed, compressed and stored on a worker pool.
- The `mapping` table records git sha → changeset, so re-running the import only ingests new commits. That makes it usable for continuously mirroring a git repository.
- Shallow-clone boundary commits start history.

Tests check that every imported commit's tree (modes, symlinks, file/directory swaps, merges) matches `git ls-tree`, and that first-parent blame matches `git blame --first-parent` line by line.

**Export (`internal/gitexport`)** goes the other way, so teams can keep git tooling (code hosts, CI that clones, code search) running against a mirror while agents work in Kudzu. `kudzu export --store DIR --git mirror.git --ref main`:

- Walks back from the ref and stops at changesets git already has: ones exported before (`mapping` namespace `git-export`, changeset → sha) or imported ones whose `extra.git` commit still maps back to them. Each candidate is checked against the target with `git cat-file --batch-check`, so exporting to a repository that lacks those commits writes them anew.
- Streams the rest, parents first, to one `git fast-import`. A changeset already lists its changes relative to its first parent, which is exactly a fast-import commit (`from`, `merge`, `M`, `D`); deletions go first so files and directories can swap. Each distinct blob is sent once. Authors map to git identities (author and committer), messages and time zones are kept.
- Moves the branch with `git update-ref NEW OLD`, a compare-and-swap, and only as a fast-forward unless `--force`.
- Records both directions in `mapping`, so the next export is incremental and a later `kudzu import` of the mirror recognizes these commits instead of importing them again.

Tests round-trip import → land (including a server-side merge and file/directory swaps) → export and compare every commit's tree, author and message with Kudzu's.

## 10. Access control (`internal/auth`, `internal/server/views.go`)

The post describes permissions on individual directories, so sensitive code can live in the monorepo. `kudzu server --auth policy.json` turns them on:

- **Authentication.** Every request but `/healthz` carries `Authorization: Bearer <token>`. The policy stores only SHA-256 hashes of tokens (`kudzu token` makes one), mapping each to a principal. Requests without a token use the optional `anonymous` principal, or get 401.
- **Rules.** Each principal has `read`, `write` and `deny` path prefixes and `land` refs. The longest matching prefix decides; at equal length deny beats write beats read. So `"read": [""], "deny": ["secrets"], "write": ["services/api"]` reads everything but `secrets/` and writes only `services/api/`.
- **Reads.** `cat`, `blame`, `ls`, `diff` and `log` with a path check read access and answer 403 naming the path. Listings, diffs, change lists and the WAL drop entries the caller may not read; a changeset that only touched such paths is hidden from `log`.
- **Writes.** Write checks happen inside `repo.Create` (through `repo.WithChangeCheck`), so they cover exactly the paths each new changeset changes, whether the server creates it for a commit, a rebase, a merge or a land's rebase. Moving a ref (land or compare-and-swap) needs a `land` entry for it.
- **Objects.** This is the hard part: objects are content-addressed and fetched by ID from any tier, with no path attached. Kudzu's answer is that a caller can only fetch what it can name, so it never learns the ID of anything it may not read. Wherever the API returns a tree (resolve, show, ls, create, rebase, merge, snapshot), a caller with restricted read access gets a *filtered tree*: the same directory with unreadable entries removed, written as an ordinary tree object. Clients verify it like any other, so clone, update, commit and land work unchanged on a filtered view. Subtrees the caller may read in full keep their real IDs, so filtering costs only the directories along rule boundaries, and the results are cached per principal. Commits are still made against the real changeset, so landing from a filtered checkout keeps the files the caller cannot see.
- **Tiers.** Caches and daemons forward each client's token on `/api/*` and uploads, and fetch objects upstream with their own `KUDZU_TOKEN`. They serve cached objects to whoever can reach them, which under this model only exposes what callers can already name. The remaining leak is an existence oracle: someone who can guess a file's exact content can confirm the repository holds it.

## 11. From the post vs Kudzu's own choices

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
- SQLite and a local directory by default, with Postgres and S3-compatible storage for running several servers, instead of Spanner and cloud storage.

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
| GET | `/api/sizes?tree=` | sizes of the files directly in a tree (for `kudzu mount`) |
| GET | `/api/ancestry?a=&b=` | merge base and ancestry |
| GET / POST | `/api/refs`, `/api/refs/{name}` | list refs; compare-and-swap a ref |
| POST | `/api/rebase`, `/api/merge`, `/api/land` | server-side rebase, merge, lock-free land |
| GET | `/api/wal?since=` | follow new changesets by log position (e.g. for a search indexer) |
| GET | `/api/snapshot?rev=&sparse=` | all objects of a revision (for seeding daemons) |
| GET / POST | `/objects/{id}`, `/objects/fetch` | object protocol, served by every tier |
| POST | `/objects/missing`, `/objects/upload` | which objects the server lacks; upload |
| GET | `/stats`, `/healthz` | statistics, health |
