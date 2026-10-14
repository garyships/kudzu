#!/usr/bin/env node
// Runs the demo scenario for real, one command at a time, and writes a JSON
// transcript: each command's text, working directory, start/end time (ms since
// the run started), exit code, and every output line with its arrival time.
// The player (player.html) only replays this file; it never invents output.
//
//   node run-scenario.mjs --kudzu ./kudzu --git ./vite --work /tmp/kd-demo --out transcript.json
//
// The work directory is wiped first. The git repository is symlinked into it
// under its base name so that commands and output show short relative paths.
import { spawn, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

const opt = {
  kudzu: 'kudzu', git: '', work: '', out: 'transcript.json',
  sparse: 'packages/vite',
  dir: 'packages/vite/src/node/server',
  file: 'packages/vite/src/node/server/index.ts',
  slug: 'vitejs/vite',
  server: 7070, cache: 7071,
  sockA: '/tmp/kudzu-a.sock', sockB: '/tmp/kudzu-b.sock',
};
for (let i = 2; i < process.argv.length; i += 2) {
  const k = process.argv[i].replace(/^--/, '');
  if (!(k in opt)) throw new Error(`unknown flag --${k}`);
  opt[k] = typeof opt[k] === 'number' ? Number(process.argv[i + 1]) : process.argv[i + 1];
}
if (!opt.git || !opt.work) {
  console.error('usage: run-scenario.mjs --kudzu BIN --git REPO --work DIR --out FILE [--sparse DIR --dir DIR --file PATH]');
  process.exit(2);
}

const work = path.resolve(opt.work);
const gitSrc = path.resolve(opt.git);
const name = path.basename(gitSrc).replace(/\.git$/, '');
fs.rmSync(work, { recursive: true, force: true });
fs.mkdirSync(work, { recursive: true });
fs.symlinkSync(gitSrc, path.join(work, name));
for (const s of [opt.sockA, opt.sockB]) fs.rmSync(s, { force: true });

const binDir = path.dirname(path.resolve(opt.kudzu));
const env = {
  ...process.env,
  PATH: `${binDir}:${process.env.PATH}`,
  KUDZU_AUTHOR: 'demo <demo@example.com>',
  GIT_TERMINAL_PROMPT: '0', LC_ALL: 'C',
};
delete env.KUDZU_REMOTE;

const T0 = performance.now();
const now = () => Math.round((performance.now() - T0) * 10) / 10;
const steps = [];
const background = [];

// Anything that still contains the absolute work path is shown relative to it.
const tidy = (s) => s.split(work + '/').join('').split(work).join('.');

function run(spec) {
  const { id, cmd, cwd = '.', pane = 'main', who = 'agent@node-1', author, bg, ready } = spec;
  const rec = { id, cmd: bg ? cmd + ' &' : cmd, cwd, pane, who, start: now(), end: null, code: null, lines: [] };
  steps.push(rec);
  const child = spawn('/bin/bash', ['-c', bg ? `exec ${cmd}` : cmd], {
    cwd: path.join(work, cwd),
    env: { ...env, ...(author ? { KUDZU_AUTHOR: author } : {}) },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let done = false;
  return new Promise((resolve, reject) => {
    const finish = (code) => {
      if (done) return;
      done = true;
      rec.end = now();
      rec.code = code;
      resolve(rec);
    };
    for (const [fd, stream] of [['out', child.stdout], ['err', child.stderr]]) {
      let buf = '';
      stream.on('data', (chunk) => {
        const t = now();
        buf += chunk.toString('utf8');
        let i;
        while ((i = buf.indexOf('\n')) >= 0) {
          const text = tidy(buf.slice(0, i));
          buf = buf.slice(i + 1);
          if (done) continue; // a background tier's later log lines are not part of this step
          rec.lines.push({ t, fd, text });
          if (bg && ready.test(text)) finish(0);
        }
      });
      stream.on('end', () => { if (buf && !done) rec.lines.push({ t: now(), fd, text: tidy(buf) }); });
    }
    child.on('error', reject);
    if (bg) {
      background.push(child);
      setTimeout(() => (done ? null : reject(new Error(`${id}: not ready after 15s`))), 15000);
      child.on('exit', (code) => { if (!done) reject(new Error(`${id} exited early (${code})`)); });
    } else {
      child.on('close', (code) => {
        finish(code);
        if (code !== 0 && !spec.mayFail) reject(new Error(`${id} failed (exit ${code}):\n${rec.lines.map((l) => l.text).join('\n')}`));
      });
    }
  });
}

// note() records a step that only changes the environment of later steps.
function note(spec) {
  const t = now();
  steps.push({ id: spec.id, cmd: spec.cmd, cwd: '.', pane: 'main', who: spec.who || 'agent@node-1', start: t, end: t, code: 0, lines: [] });
  Object.assign(env, spec.env);
}

function portFree(port) {
  return new Promise((resolve) => {
    const s = net.connect(port, '127.0.0.1');
    s.on('connect', () => { s.destroy(); resolve(false); });
    s.on('error', () => resolve(true));
  });
}

const sh = (cmd, args, cwd) => execFileSync(cmd, args, { cwd, encoding: 'utf8' }).trim();

async function main() {
  for (const p of [opt.server, opt.cache]) {
    if (!(await portFree(p))) throw new Error(`port ${p} is in use; pass --server/--cache`);
  }
  const A = `unix://${opt.sockA}`;
  const B = `unix://${opt.sockB}`;
  const S = `127.0.0.1:${opt.server}`;
  const C = `127.0.0.1:${opt.cache}`;

  // 1. import
  await run({ id: 'import', cmd: `kudzu import --store store --git ${name} --ref HEAD --as main` });

  // 2. three tiers
  await run({ id: 'server', cmd: `kudzu server --store store --listen ${S}`, bg: true, ready: /listening on/ });
  await run({ id: 'cache', cmd: `kudzu cache --upstream ${S} --listen ${C}`, bg: true, ready: /listening on/ });
  await run({ id: 'daemon', cmd: `kudzu daemon --upstream ${C} --listen ${A} --dir daemon-a`, bg: true, ready: /listening on/ });
  note({ id: 'export', cmd: `export KUDZU_REMOTE=${A}`, env: { KUDZU_REMOTE: A } });

  // 3. history without a checkout
  await run({ id: 'log', cmd: 'kudzu log -n 5 --oneline' });
  await run({ id: 'logpath', cmd: `kudzu log -n 4 --oneline ${opt.dir}` });
  await run({ id: 'blame', cmd: `kudzu blame ${opt.file} | head -6` });

  // 4. clone race (run one after another, shown side by side)
  await run({ id: 'clone_sparse', pane: 'left', cmd: `kudzu clone --sparse ${opt.sparse} $KUDZU_REMOTE agent-1` });
  await run({ id: 'git_full', pane: 'right', cmd: `git clone --no-local ${name} git-full` });
  await run({ id: 'clone_full', pane: 'left', cmd: 'kudzu clone $KUDZU_REMOTE agent-2' });
  await run({ id: 'git_shallow', pane: 'right', cmd: `git clone --depth 1 file://$PWD/${name} git-shallow` });

  // 5. two agents edit the same file and land at the same time
  const a1 = { pane: 'left', cwd: 'agent-1', who: 'agent-1@node-1', author: 'agent-1 <agent-1@example.com>' };
  const a2 = { pane: 'right', cwd: 'agent-2', who: 'agent-2@node-1', author: 'agent-2 <agent-2@example.com>' };
  await run({ ...a1, id: 'edit1', cmd: `sed -i '2i // agent-1: guard restarts' ${opt.file}` });
  await run({ ...a2, id: 'edit2', cmd: `echo '// agent-2: log close()' >> ${opt.file}` });
  await run({ ...a1, id: 'status1', cmd: 'kudzu status' });
  await run({ ...a2, id: 'status2', cmd: 'kudzu status' });
  await Promise.all([
    run({ ...a1, id: 'land1', cmd: 'kudzu commit -m "server: guard restarts" --land main' }),
    run({ ...a2, id: 'land2', cmd: 'kudzu commit -m "server: log close()" --land main' }),
  ]);
  await Promise.all([
    run({ ...a1, id: 'update1', cmd: 'kudzu update' }),
    run({ ...a2, id: 'update2', cmd: 'kudzu update' }),
  ]);
  await run({ ...a1, id: 'log3', cmd: 'kudzu log -n 3 --oneline' });
  await run({ ...a2, id: 'blamegrep', cmd: `kudzu blame ${opt.file} | grep agent-` });

  // 6. writes through the API, no checkout
  const bot = { author: 'release-bot <bot@example.com>' };
  await run({ ...bot, id: 'note', cmd: `echo 'Written through the API, no checkout.' > note.md` });
  await run({ ...bot, id: 'write', cmd: 'kudzu write -m "docs: add demo note" --land main docs/demo-note.md=@note.md' });
  await run({ ...bot, id: 'diffstat', cmd: 'kudzu diff --stat --to main' });
  await run({
    ...bot, id: 'burst',
    cmd: 'for i in $(seq 16); do kudzu write -m "bot $i" --land main bots/bot-$i.md=@note.md | grep landed & done; wait',
  });

  // 7. a second machine: eight CI jobs start at once on a cold daemon
  const ci = { who: 'ci@node-2' };
  await run({ ...ci, id: 'daemon_b', cmd: `kudzu daemon --upstream ${C} --listen ${B} --dir daemon-b`, bg: true, ready: /listening on/ });
  note({ ...ci, id: 'export_b', cmd: `export KUDZU_REMOTE=${B}`, env: { KUDZU_REMOTE: B } });
  await run({
    ...ci, id: 'ci',
    cmd: `for i in $(seq 8); do kudzu clone --sparse ${opt.sparse} $KUDZU_REMOTE ci-$i 2>/dev/null & done; wait`,
  });
  await run({ ...ci, id: 'stats_b', cmd: `kudzu stats | grep -E 'objects_served|coalesced|upstream_(req|obj)'` });
  await run({ ...ci, id: 'stats_c', cmd: `kudzu stats --remote ${C} | grep -E '"(hits|misses)"'` });
  await run({ ...ci, id: 'stats_s', cmd: `kudzu stats --remote ${S} | grep land` });

  // Full JSON stats for the record (not typed on screen).
  const stats = {};
  for (const [k, r] of [['daemon_a', A], ['daemon_b', B], ['cache', C], ['server', S]]) {
    stats[k] = JSON.parse(sh(path.resolve(opt.kudzu), ['stats', '--remote', r], work));
  }
  const files = Number(sh('git', ['-C', gitSrc, 'ls-tree', '-r', '--name-only', 'HEAD']).split('\n').length);
  const sparseFiles = Number(sh('git', ['-C', gitSrc, 'ls-tree', '-r', '--name-only', 'HEAD', opt.sparse]).split('\n').length);
  const kudzuRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), '../..');
  let kudzuCommit = '';
  try { kudzuCommit = sh('git', ['-C', kudzuRoot, 'rev-parse', '--short=12', 'HEAD']); } catch {}
  const cpu = os.cpus();
  return {
    meta: {
      recorded_at: new Date().toISOString(),
      subject: {
        slug: opt.slug, name,
        head: sh('git', ['-C', gitSrc, 'rev-parse', 'HEAD']),
        commits: Number(sh('git', ['-C', gitSrc, 'rev-list', '--count', 'HEAD'])),
        files, sparse: opt.sparse, sparse_files: sparseFiles, file: opt.file,
      },
      kudzu_commit: kudzuCommit,
      host: { cpus: cpu.length, cpu: cpu[0]?.model, mem_gb: Math.round(os.totalmem() / 2 ** 30), platform: `${os.type()} ${os.release()}` },
      git: sh('git', ['--version']),
      go: (() => { try { return sh('go', ['version']); } catch { return ''; } })(),
      stats,
    },
    steps,
  };
}

let code = 0;
try {
  const transcript = await main();
  fs.writeFileSync(opt.out, JSON.stringify(transcript, null, 1));
  for (const s of transcript.steps) {
    console.log(`${String(((s.end - s.start) / 1000).toFixed(3)).padStart(7)}s  ${s.cmd}`);
  }
  console.log(`wrote ${opt.out}`);
} catch (e) {
  console.error(e.message || e);
  code = 1;
} finally {
  for (const c of background) c.kill('SIGTERM');
  for (const s of [opt.sockA, opt.sockB]) fs.rmSync(s, { force: true });
}
setTimeout(() => process.exit(code), 300);
