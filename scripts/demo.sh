#!/usr/bin/env bash
# End-to-end demo: import a git repository, run server -> cache -> daemon,
# then clone, commit and land the way agents would.
#
#   scripts/demo.sh /path/to/a/git/repo [sparse/dir]
set -euo pipefail

GIT_REPO=$(cd "${1:?usage: demo.sh /path/to/git/repo [sparse/dir]}" && pwd)
SPARSE_DIR=${2:-}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/kd.XXXXXX")
SOCK="$WORK/d.sock"
# Unix socket paths are limited to ~108 bytes.
[ ${#SOCK} -lt 100 ] || SOCK=$(mktemp -u /tmp/kudzu.XXXXXX.sock)
BIN="$WORK/kudzu"
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
timed() { local s=$(date +%s.%N); "$@"; local e=$(date +%s.%N); printf '   (%.2fs)\n' "$(echo "$e - $s" | bc)"; }

step "build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/kudzu)

step "import git history from $GIT_REPO"
timed "$BIN" import --store "$WORK/store" --git "$GIT_REPO" --ref HEAD --as main

step "start server, regional cache, node daemon"
"$BIN" server --store "$WORK/store" --listen 127.0.0.1:17070 2>"$WORK/server.log" & PIDS+=($!)
"$BIN" cache --upstream 127.0.0.1:17070 --listen 127.0.0.1:17071 2>"$WORK/cache.log" & PIDS+=($!)
"$BIN" daemon --upstream 127.0.0.1:17071 --listen "unix://$SOCK" --dir "$WORK/daemon" 2>"$WORK/daemon.log" & PIDS+=($!)
for _ in $(seq 50); do [ -S "$SOCK" ] && break; sleep 0.1; done
export KUDZU_REMOTE="unix://$SOCK" KUDZU_AUTHOR="Demo Agent <agent@example.com>"

if [ -z "$SPARSE_DIR" ]; then
  SPARSE_DIR=$("$BIN" ls -r main | awk '$3 ~ /\/$/ {print $3}' | head -1 | tr -d /)
fi

step "history queries with no checkout at all"
"$BIN" log -r main -n 5 --oneline
FIRST_FILE=$("$BIN" ls -r main "$SPARSE_DIR" | awk '$3 !~ /\/$/ {print $3; exit}')
if [ -n "$FIRST_FILE" ]; then
  echo "-- path history of $SPARSE_DIR/$FIRST_FILE"
  "$BIN" log -r main --oneline -n 5 "$SPARSE_DIR/$FIRST_FILE"
  echo "-- blame (first lines)"
  "$BIN" blame -r main "$SPARSE_DIR/$FIRST_FILE" | head -5 || true
fi

step "sparse clone of $SPARSE_DIR (cold daemon)"
timed "$BIN" clone --sparse "$SPARSE_DIR" "$KUDZU_REMOTE" "$WORK/agent1"
step "full clone (warm daemon)"
timed "$BIN" clone "$KUDZU_REMOTE" "$WORK/agent2"
step "a second machine: new daemon, same regional cache"
SOCK2="${SOCK%.sock}2.sock"
"$BIN" daemon --upstream 127.0.0.1:17071 --listen "unix://$SOCK2" --dir "$WORK/daemon2" 2>"$WORK/daemon2.log" & PIDS+=($!)
for _ in $(seq 50); do [ -S "$SOCK2" ] && break; sleep 0.1; done
timed "$BIN" clone "unix://$SOCK2" "$WORK/agent3"
step "for comparison: full git clone of the same repository"
timed git clone -q --no-local "$GIT_REPO" "$WORK/gitclone"

step "two agents commit and land concurrently (no merge queue lock)"
echo "// agent 1 was here" >> "$WORK/agent1/$SPARSE_DIR/kudzu-demo-1.txt"
echo "agent 2 was here" > "$WORK/agent2/KUDZU_DEMO_2.txt"
(cd "$WORK/agent1" && "$BIN" commit -A -m "agent 1: add demo file" --land main) & A1=$!
(cd "$WORK/agent2" && "$BIN" commit -A -m "agent 2: add demo file" --land main) & A2=$!
wait "$A1" "$A2"
"$BIN" log -r main -n 3 --oneline

step "agent 1 catches up (fetches only what changed)"
(cd "$WORK/agent1" && "$BIN" update -r main)

step "write through the API without any checkout"
echo "written without a checkout" > "$WORK/note.txt"
"$BIN" write -r main -m "api-only write" --land main "notes/api-write.txt=@$WORK/note.txt"
"$BIN" diff --stat --to main

step "tier statistics"
echo "-- daemon";  "$BIN" stats --remote "unix://$SOCK"  | grep -E '"(hits|misses|hit_rate|coalesced_misses|upstream_requests|upstream_objects)"'
echo "-- daemon 2"; "$BIN" stats --remote "unix://$SOCK2" | grep -E '"(hits|misses|hit_rate|upstream_objects)"'
echo "-- cache";   "$BIN" stats --remote 127.0.0.1:17071        | grep -E '"(hits|misses|hit_rate|upstream_objects)"'
echo "-- server";  "$BIN" stats --remote 127.0.0.1:17070        | grep -E '"(objects_served|changesets_created|lands|land_retries)"'

step "sizes"
du -sh "$WORK/agent1" "$WORK/agent2" "$WORK/gitclone" 2>/dev/null
du -sh "$WORK/store" "$GIT_REPO/.git" 2>/dev/null
echo "work dir: $WORK"
