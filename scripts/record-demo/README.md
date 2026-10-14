# Demo video

Regenerates `docs/assets/demo.mp4`, `docs/assets/demo.gif` and `docs/assets/demo-poster.png` from a real run of Kudzu:

```bash
scripts/record-demo/make.sh
```

Requirements: Go, git, ffmpeg, Node 18+ with the `playwright` package installed globally (`npm i -g playwright`) and its Chromium. The run clones `vitejs/vite` (last 1,000 commits) unless `SUBJECT_DIR` points at an existing clone. It needs ports 7070/7071 and the sockets `/tmp/kudzu-a.sock` and `/tmp/kudzu-b.sock`. A full regeneration takes about 8 minutes on 4 vCPUs.

| Step | File | What it does |
|---|---|---|
| 1 | `run-scenario.mjs` | Runs each command for real (import, three tiers, history queries, clones, `git clone` for comparison, two concurrent `--land`s, API writes, a second machine with eight CI clones, tier stats) and writes `transcript.json` with every output line and its arrival time |
| 2 | `player.html` | Replays a transcript as a 1920x1080 page. `window.renderAt(t)` is a pure function of time. Terminal text, timing badges, charts and caption numbers are all read from the transcript |
| 3 | `record.mjs` | Steps `renderAt` frame by frame in headless Chromium and saves PNG frames (`--times` for stills, `--preview out.html` for a page you can open in a browser) |
| 4 | `make.sh` | Runs 1 to 3 and encodes the MP4 (H.264, yuv420p, faststart), the poster (title card) and the GIF (clone race and concurrent land, 960 px, 12 fps) |

Honesty rules the player follows:

- Output is shown as recorded. Display paths are relative to the work directory, which the runner arranges by running everything there; it only rewrites a path if a tool prints the absolute work directory.
- Output appears with its recorded timing. When a step is shown faster or slower than real time (the import at 3x, the clone race in 1/3 slow motion), a badge says so, and the timers still show real wall-clock time.
- Clone timings come from separate, sequential runs replayed side by side. The caption comparing a full Kudzu checkout with `git clone --depth 1` is worded from the measured numbers, whichever way they go.
- Everything runs on one machine, so numbers vary from run to run. They are illustrative, not a benchmark, and the video says so.

Environment variables for `make.sh`: `WORK` (scratch directory, kept afterwards; holds the transcript and frames), `SUBJECT_URL`, `SUBJECT_DIR`, `DEPTH`, `OUT` (default `docs/assets`), `FPS` (default 30), and `TRANSCRIPT` (skip the run and re-render an existing transcript, for changes to the player only). Transcripts contain no absolute paths from the work directory, but they stay in `WORK`. Don't commit them.
