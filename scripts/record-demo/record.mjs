#!/usr/bin/env node
// Renders player.html frame by frame with Playwright (Chromium) and writes
// PNG frames plus timeline.json (scene boundaries). make.sh encodes them.
//
//   node record.mjs --transcript t.json --frames DIR [--fps 30] [--jobs 3]
//   node record.mjs --transcript t.json --frames DIR --times 2,15.5,40   # stills only
//   node record.mjs --transcript t.json --preview preview.html           # playable page
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { execSync } from 'node:child_process';

const opt = { transcript: '', frames: '', fps: 30, jobs: 3, times: '', preview: '', width: 1920, height: 1080 };
for (let i = 2; i < process.argv.length; i += 2) {
  const k = process.argv[i].replace(/^--/, '');
  if (!(k in opt)) throw new Error(`unknown flag --${k}`);
  opt[k] = typeof opt[k] === 'number' ? Number(process.argv[i + 1]) : process.argv[i + 1];
}
if (!opt.transcript || !(opt.frames || opt.preview)) {
  console.error('usage: record.mjs --transcript FILE (--frames DIR [--fps 30] [--jobs 3] [--times a,b,c] | --preview OUT.html)');
  process.exit(2);
}
const here = path.dirname(new URL(import.meta.url).pathname);
const transcript = fs.readFileSync(opt.transcript, 'utf8');
if (opt.preview) {
  const html = fs.readFileSync(path.join(here, 'player.html'), 'utf8')
    .replace('<script>\n', () => `<script>window.TRANSCRIPT = ${transcript.replace(/<\//g, '<\\/')};</script>\n<script>\n`);
  fs.writeFileSync(opt.preview, html);
  console.log(`wrote ${opt.preview}`);
  process.exit(0);
}

// playwright is used as a global module (npm i -g playwright); find it.
const require = createRequire(import.meta.url);
let chromium;
try {
  ({ chromium } = require('playwright'));
} catch {
  const root = execSync('npm root -g', { encoding: 'utf8' }).trim();
  ({ chromium } = require(path.join(root, 'playwright')));
}

const player = 'file://' + path.join(here, 'player.html');
fs.mkdirSync(opt.frames, { recursive: true });

const browser = await chromium.launch();
async function openPage() {
  const page = await browser.newPage({ viewport: { width: opt.width, height: opt.height }, deviceScaleFactor: 1 });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  await page.addInitScript(`window.TRANSCRIPT = ${transcript}; window.FRAME_STEPPING = true;`);
  await page.goto(player);
  await page.evaluate(() => document.fonts.ready);
  if (errors.length) throw new Error('player error: ' + errors.join('\n'));
  return page;
}

try {
  const first = await openPage();
  const duration = await first.evaluate(() => window.DURATION);
  const scenes = await first.evaluate(() => window.SCENES);
  fs.writeFileSync(path.join(opt.frames, 'timeline.json'), JSON.stringify({ fps: opt.fps, duration, scenes }, null, 1));

  if (opt.times) {
    for (const t of opt.times.split(',').map(Number)) {
      await first.evaluate((x) => window.renderAt(x), t);
      const f = path.join(opt.frames, `still_${t.toFixed(2)}.png`);
      await first.screenshot({ path: f });
      console.log(f);
    }
  } else {
    const n = Math.ceil(duration * opt.fps);
    const pages = [first];
    for (let j = 1; j < opt.jobs; j++) pages.push(await openPage());
    let done = 0;
    const started = Date.now();
    await Promise.all(pages.map(async (page, j) => {
      for (let i = j; i < n; i += pages.length) {
        await page.evaluate((x) => window.renderAt(x), i / opt.fps);
        await page.screenshot({ path: path.join(opt.frames, `f_${String(i).padStart(5, '0')}.png`) });
        if (++done % 300 === 0) console.log(`  ${done}/${n} frames (${((Date.now() - started) / 1000).toFixed(0)} s)`);
      }
    }));
    console.log(`rendered ${n} frames (${duration.toFixed(2)} s at ${opt.fps} fps) into ${opt.frames}`);
  }
} finally {
  await browser.close();
}
