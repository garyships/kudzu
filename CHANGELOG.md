# Changelog

Pushing a `v*` tag publishes that version's section below as its release notes.

## v0.2.0 (2026-11-08)

Access control, shared backends, a git mirror, FUSE checkouts you can edit, and the fixes from a security review of every tier. Upgrade if you run v0.1.0: several fixes below close holes it has.

### New

- **Access control.** `kudzu server --auth POLICY` requires bearer tokens (`kudzu token`) and enforces per-directory read, write and land rules. Restricted readers get filtered trees, logs and diffs, and ref moves are checked against their content. (#8, #11)
- **Several servers on shared storage.** Stateless servers share Postgres (`--meta postgres://…`) and an S3-compatible bucket (`--objects s3://…`; also GCS, MinIO and R2). (#8)
- **`kudzu export`** keeps a git mirror of a ref in sync, incrementally. (#8)
- **`kudzu gc --store`** collects a server store's garbage by mark and sweep, safely while changesets land. (#8, #12)
- **`kudzu mount`** mounts a revision with FUSE and fetches each file on first read. With `--writable` you can edit it and run `status`, `diff`, `commit --land`, `update` and `land` inside it. Build with `-tags fuse`. (#8, #13, #22)
- **Revision syntax:** `rev^N` and chained suffixes such as `main~2^2`. (#8)
- Faster commands inside a working copy, and a faster `status`; `kudzu refs` is sorted. (#21)

### Security

Found by a review of every tier and reproduced before being fixed.

- **`kudzu up` exposed its tiers to every local user:** its server and cache listened on loopback TCP without authentication. They now listen on owner-only unix sockets in a private directory. Every tier also refuses cross-origin browser writes and, on loopback, the Host names a DNS-rebinding page would use, and clients no longer follow redirects. (#27)
- **Forged merge parents** let a principal with only a land rule make someone else's pending changeset count as landed, so its land did nothing. Principals whose writes are checked now merge through `/api/merge`. Creating, rebasing and merging need write access somewhere, and changesets and request bodies are bounded and permission-checked before they are read. (#31)
- **Token theft through a planted `.kudzu/mount.json`** in a shared directory such as `/tmp`. Only the nearest `.kudzu` is used, it must be a private directory of yours, and a mount's control socket must be yours. (#32)
- **Planted working copies and sockets:** a `.kudzu` or a daemon socket that another user made in a shared directory is refused, and the daemon's socket is owner-only by default. (#16, #18)
- **`kudzu export` injection** through a submodule blob that was not a commit ID. (#25)
- **Server memory exhaustion** from blame or a diff of a small upload that inflates to millions of lines. (#33)
- **Bloated copies of existing blobs** through padded stored forms. (#26)
- **Internal errors** no longer reach clients. They could name storage keys, and so the IDs of trees hidden from restricted readers. (#31, #34)
- **Hostile trees** can no longer write outside a checkout, and `update` removes only files the working copy tracks. (#20, #29)
- **Cache denial of service:** abandoned fetches are cancelled and misses in flight are capped. (#34)
- **Object protocol:** size limits, verified caching, broken-stream detection and timeouts. (#19)
- **No more predictable directories in `/tmp`** when there is no user cache directory. (#28)

### Changed

These may need a change on your side.

- `kudzu up` no longer listens on TCP; `--port` is accepted and ignored. Use the unix sockets it prints, or the daemon's.
- With `--auth`, a principal that may not write everything cannot create a changeset with several parents (use `/api/merge`), and creating, rebasing and merging need a write rule.
- Errors: a 500 says only `internal error (request <id>)`, with the details in the server log under that ID. An oversized body gets 413, and a land that runs out of attempts gets 503.
- Files over 8 MiB or 200,000 lines are not blamed (400), show as one line in patches, and conflict when both sides change them.
- A working copy whose `.kudzu` other users can write to is refused (`chmod go-w` it, or set `KUDZU_SAFE_DIRECTORY`).
- The daemon, `kudzu up` and writable mounts need `--dir` or `--overlay` when there is no user cache directory.
- Re-importing from git keeps work landed in Kudzu: the ref only fast-forwards unless `--force`. (#17)

### Fixed

- The server's object writes are durable before the metadata that names them. (#15)
- A shared derived-data computation survives the caller that started it hanging up, and stops once nobody waits. (#14, #33)
- A mounted file is read once per open, not once per chunk. (#13)
- Writable mounts no longer lose files named `*.kudzu-tmp`. (#30)
- A submodule removed upstream no longer breaks `update`. (#29)

### Project

- CI runs on Go 1.26 and stable, on Linux, macOS and Windows, with FUSE, Postgres and S3, staticcheck and govulncheck, and runs faster: parallel tests and a cache per job. (#9, #23)

## v0.1.0 (2026-10-14)

First public release.
