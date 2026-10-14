#!/usr/bin/env bash
# Regenerates docs/assets/demo.mp4, demo.gif and demo-poster.png from a real
# run of Kudzu: build, run the scenario (run-scenario.mjs), replay the
# transcript frame by frame (player.html + record.mjs), encode with ffmpeg.
#
#   scripts/record-demo/make.sh
#
# Environment:
#   WORK=dir          scratch directory (default: a new mktemp dir; kept afterwards)
#   SUBJECT_URL=url   git repository to import (default: https://github.com/vitejs/vite)
#   SUBJECT_DIR=dir   use an existing local clone instead of cloning SUBJECT_URL
#   DEPTH=n           history depth for the subject clone (default: 1000)
#   OUT=dir           output directory (default: docs/assets)
#   FPS=n             frame rate of the MP4 (default: 30)
#   TRANSCRIPT=file   skip building and running; re-render an existing transcript
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
WORK=${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/kudzu-video.XXXXXX")}
SUBJECT_URL=${SUBJECT_URL:-https://github.com/vitejs/vite}
DEPTH=${DEPTH:-1000}
OUT=${OUT:-$ROOT/docs/assets}
FPS=${FPS:-30}
mkdir -p "$WORK" "$OUT"
echo "work dir: $WORK"

if [ -z "${TRANSCRIPT:-}" ]; then
  echo "== build kudzu"
  (cd "$ROOT" && go build -o "$WORK/bin/kudzu" ./cmd/kudzu)

  SUBJECT=${SUBJECT_DIR:-$WORK/src/$(basename "$SUBJECT_URL" .git)}
  if [ ! -d "$SUBJECT/.git" ]; then
    echo "== clone $SUBJECT_URL (depth $DEPTH)"
    mkdir -p "$(dirname "$SUBJECT")"
    git clone -q --depth "$DEPTH" "$SUBJECT_URL" "$SUBJECT"
  fi

  echo "== run the scenario for real"
  TRANSCRIPT=$WORK/transcript.json
  node "$HERE/run-scenario.mjs" --kudzu "$WORK/bin/kudzu" --git "$SUBJECT" \
    --work "$WORK/run" --out "$TRANSCRIPT" \
    --slug "$(echo "$SUBJECT_URL" | sed -E 's#^https?://github.com/##; s#\.git$##')"
fi

echo "== render frames"
rm -rf "$WORK/frames"
node "$HERE/record.mjs" --transcript "$TRANSCRIPT" --frames "$WORK/frames" --fps "$FPS"

echo "== encode"
ffmpeg -loglevel error -y -framerate "$FPS" -i "$WORK/frames/f_%05d.png" \
  -c:v libx264 -preset slow -crf 16 -tune animation -pix_fmt yuv420p -movflags +faststart \
  "$OUT/demo.mp4"

# Poster: the title card (static from t=0, so it matches the video's first frame).
ffmpeg -loglevel error -y -i "$WORK/frames/f_$(printf %05d $((FPS * 2))).png" -frames:v 1 "$OUT/demo-poster.png"

# GIF: the clone race and the two-agent land, at 12 fps, 960 px wide.
read -r GSTART GDUR < <(node -e '
  const t = require(process.argv[1]);
  const s = (k) => t.scenes.find((x) => x.key === k);
  const a = s("clone").start, b = s("land").end - 0.6;
  console.log(Math.round(a * t.fps), Math.min(b - a, 30).toFixed(2));' "$WORK/frames/timeline.json")
GIF_IN=(-framerate "$FPS" -start_number "$GSTART" -t "$GDUR" -i "$WORK/frames/f_%05d.png")
ffmpeg -loglevel error -y "${GIF_IN[@]}" \
  -vf "fps=12,scale=960:-1:flags=lanczos,palettegen=max_colors=256:stats_mode=diff" "$WORK/palette.png"
ffmpeg -loglevel error -y "${GIF_IN[@]}" -i "$WORK/palette.png" \
  -lavfi "fps=12,scale=960:-1:flags=lanczos[x];[x][1:v]paletteuse=dither=none:diff_mode=rectangle" \
  -loop 0 "$OUT/demo.gif"

for f in demo.mp4 demo.gif demo-poster.png; do
  printf '%-16s %6s  %s\n' "$f" "$(du -h "$OUT/$f" | cut -f1)" \
    "$(ffprobe -v error -show_entries format=duration -show_entries stream=width,height -of csv=p=0 "$OUT/$f" | tr '\n' ' ')"
done
echo "transcript: $TRANSCRIPT"
