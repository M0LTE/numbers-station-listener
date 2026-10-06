// Browser playback check for internal/webm. Builds and starts the Go sample
// server, then in each available browser loads each stream into an <audio>
// element, calls play(), and waits for currentTime to pass a target with no
// MediaError. Manual tool: it uses real time and real browsers, so it is not
// part of `go test`. See README.md in this directory.
//
//   cd scripts/webm-playback && npm install && node check.mjs [serve flags]
//
// Browsers: Playwright's Chromium from ~/.cache/ms-playwright (newest
// chromium-*), /usr/bin/google-chrome, and Playwright's Firefox if installed
// (`npx playwright-core install firefox`). Firefox needs an audio server to
// play at all; set PULSE_SERVER (pulse-null.sh starts a throwaway one).
// Exit status is non-zero if any browser that launched fails a stream that
// is not marked as a diagnostic.

import { chromium, firefox } from 'playwright-core';
import { execFileSync, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readdirSync, rmSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const repo = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const serveArgs = process.argv.slice(2).filter((a) => a !== '--');

function playwrightChromium() {
  const cache = join(homedir(), '.cache', 'ms-playwright');
  if (!existsSync(cache)) return null;
  const dirs = readdirSync(cache).filter((d) => /^chromium-\d+$/.test(d)).sort().reverse();
  for (const d of dirs) {
    const p = join(cache, d, 'chrome-linux64', 'chrome');
    if (existsSync(p)) return { label: `playwright ${d}`, path: p };
  }
  return null;
}

const autoplay = ['--autoplay-policy=no-user-gesture-required'];
const candidates = [];
const pwc = playwrightChromium();
if (pwc) candidates.push({ name: `Chromium (${pwc.label})`, launch: () => chromium.launch({ executablePath: pwc.path, args: autoplay }) });
else candidates.push({ name: 'Chromium (playwright)', missing: 'no chromium-* under ~/.cache/ms-playwright' });
if (existsSync('/usr/bin/google-chrome')) candidates.push({ name: 'Google Chrome', launch: () => chromium.launch({ executablePath: '/usr/bin/google-chrome', args: autoplay }) });
else candidates.push({ name: 'Google Chrome', missing: '/usr/bin/google-chrome not found' });
candidates.push({
  name: 'Firefox (playwright)',
  launch: () => firefox.launch({
    firefoxUserPrefs: { 'media.autoplay.default': 0, 'media.autoplay.blocking_policy': 0 },
    env: { ...process.env },
  }),
  note: process.env.PULSE_SERVER ? null : 'PULSE_SERVER unset: expect OnMediaSinkAudioError (no audio device)',
});

// mode "element": <audio src>. mode "mse": fetch() the stream and append
// each chunk to a MediaSource SourceBuffer, the low-latency alternative.
// diag: reported but never counted as a failure.
const streams = [
  { path: '/static.webm', mode: 'element', target: 2.0, timeoutMs: 15000 },
  { path: '/silence.webm', mode: 'element', target: 2.0, timeoutMs: 15000 },
  // Live: chunked, growing, paced in real time, no backlog on join. Plays
  // for 20 s to show it keeps up, and how far behind live it runs.
  { path: '/live.webm', mode: 'element', target: 20.0, timeoutMs: 45000 },
  // Live with 6 s of backlog sent at once on join.
  { path: '/live.webm?burst=300', mode: 'element', target: 3.0, timeoutMs: 25000 },
  // Live with 3-byte packets: start-up waits on bytes, not time.
  { path: '/live.webm?src=silence', mode: 'element', target: 1.0, timeoutMs: 12000, diag: true },
  // Live through MSE, with the default and with 100 ms clusters.
  { path: '/live.webm', mode: 'mse', target: 20.0, timeoutMs: 45000 },
  { path: '/live.webm?cluster=100ms', mode: 'mse', target: 10.0, timeoutMs: 30000 },
];

function startServer() {
  return new Promise((resolve, reject) => {
    // Build then run the binary directly, so killing it really stops it.
    const dir = mkdtempSync(join(tmpdir(), 'webm-check-'));
    const bin = join(dir, 'serve');
    execFileSync('go', ['build', '-o', bin, './scripts/webm-playback/serve'], { cwd: repo, stdio: 'inherit' });
    const p = spawn(bin, serveArgs, { cwd: repo, stdio: ['ignore', 'pipe', 'inherit'] });
    p.on('exit', () => rmSync(dir, { recursive: true, force: true }));
    p.on('error', reject);
    p.once('exit', (code) => reject(new Error(`server exited ${code}`)));
    let buf = '';
    p.stdout.on('data', (d) => {
      buf += d;
      const m = buf.match(/listening on (http:\/\/\S+)/);
      if (m) resolve({ proc: p, base: m[1] });
    });
  });
}

// Runs in the page.
async function probe({ src, mode, target, timeoutMs }) {
  const a = document.getElementById('a');
  const t0 = performance.now();
  const now = () => Math.round(performance.now() - t0);
  const events = [];
  let waiting = 0;
  for (const e of ['loadedmetadata', 'canplay', 'playing', 'waiting', 'stalled', 'error', 'ended']) {
    a.addEventListener(e, () => {
      if (e === 'waiting') waiting++;
      if (events.length < 16) events.push(`${e}@${now()}ms`);
    });
  }
  let firstDuration = null;
  let firstPlaying = null;
  a.addEventListener('loadedmetadata', () => { firstDuration = String(a.duration); }, { once: true });
  a.addEventListener('playing', () => { if (firstPlaying === null) firstPlaying = now(); });
  let mseError = null;
  let ctrl = null;
  if (mode === 'mse') {
    const type = 'audio/webm; codecs="opus"';
    if (!window.MediaSource || !MediaSource.isTypeSupported(type)) return { ok: false, error: `MSE ${type} unsupported` };
    const ms = new MediaSource();
    a.src = URL.createObjectURL(ms);
    await new Promise((r) => ms.addEventListener('sourceopen', r, { once: true }));
    const sb = ms.addSourceBuffer(type);
    ctrl = new AbortController();
    (async () => {
      try {
        const res = await fetch(src, { signal: ctrl.signal });
        const reader = res.body.getReader();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          if (sb.updating) await new Promise((r) => sb.addEventListener('updateend', r, { once: true }));
          sb.appendBuffer(value);
        }
      } catch (e) {
        if (e.name !== 'AbortError') mseError = String(e);
      }
    })();
  } else {
    a.src = src;
  }
  let playError = null;
  try { await a.play(); } catch (e) { playError = String(e); }
  while (!a.error && !mseError && a.currentTime < target && performance.now() - t0 < timeoutMs) {
    await new Promise((r) => setTimeout(r, 100));
  }
  const ranges = (tr) => Array.from({ length: tr.length }, (_, i) => [+tr.start(i).toFixed(3), +tr.end(i).toFixed(3)]);
  const result = {
    ok: !a.error && !playError && !mseError && a.currentTime >= target,
    currentTime: +a.currentTime.toFixed(3),
    // For live streams (no backlog) the server produces audio in real time
    // from the request, so this is how far behind live the listener hears.
    behindLiveSec: +((performance.now() - t0) / 1000 - a.currentTime).toFixed(2),
    durationAtMetadata: firstDuration,
    durationNow: String(a.duration),
    paused: a.paused,
    readyState: a.readyState,
    error: a.error ? `code ${a.error.code}: ${a.error.message}` : (mseError ?? null),
    playError,
    firstPlayingMs: firstPlaying,
    waitingEvents: waiting,
    wallMs: now(),
    buffered: ranges(a.buffered),
    seekable: ranges(a.seekable),
    events,
  };
  if (ctrl) ctrl.abort();
  a.removeAttribute('src');
  a.load();
  return result;
}

const { proc, base } = await startServer();
let failures = 0;
const summary = [];
try {
  for (const c of candidates) {
    if (c.missing) { summary.push(`${c.name}: NOT AVAILABLE (${c.missing})`); continue; }
    let browser;
    try {
      browser = await c.launch();
    } catch (e) {
      summary.push(`${c.name}: NOT AVAILABLE (launch failed: ${String(e).split('\n')[0]})`);
      continue;
    }
    const version = browser.version();
    // A browser that cannot load the page at all is an environment
    // problem (seen: a sandbox denying Chrome its sockets), not a muxer one.
    try {
      const page = await browser.newPage();
      await page.goto(base + '/');
      await page.close();
    } catch (e) {
      summary.push(`${c.name} ${version}: NOT AVAILABLE (cannot load the local server: ${String(e).split('\n')[0]})`);
      await browser.close();
      continue;
    }
    if (c.note) summary.push(`${c.name} ${version}: NOTE ${c.note}`);
    for (const s of streams) {
      const page = await browser.newPage();
      let r;
      try {
        await page.goto(base + '/');
        r = await page.evaluate(probe, { src: base + s.path, mode: s.mode, target: s.target, timeoutMs: s.timeoutMs });
      } catch (e) {
        r = { ok: false, error: 'harness: ' + String(e).split('\n')[0] };
      }
      await page.close();
      const verdict = r.ok ? 'PASS' : s.diag ? 'INFO' : 'FAIL';
      if (!r.ok && !s.diag) failures++;
      const label = `${c.name} ${version} ${s.mode} ${s.path}`;
      console.log(`\n== ${label}: ${verdict}`);
      console.log(JSON.stringify(r));
      summary.push(`${label}: ${verdict} currentTime=${r.currentTime} firstPlaying=${r.firstPlayingMs}ms behindLive=${r.behindLiveSec}s waiting=${r.waitingEvents} duration=${r.durationNow} error=${r.error ?? r.playError ?? 'none'}`);
    }
    await browser.close();
  }
} finally {
  proc.kill();
}
console.log('\nSummary:\n' + summary.map((l) => '  ' + l).join('\n'));
process.exit(failures ? 1 : 0);
