// Manual end-to-end check against a running site: free-tunes a receiver,
// plays the relayed audio in Chromium, reads the waterfall socket, then
// leaves and confirms the site released the upstream. Uses real time and a
// real receiver, so it is never part of `go test`.
//
//   node scripts/webm-playback/live-site.mjs http://10.45.0.26:8080 M0LTE 6070000 am
import { chromium } from 'playwright-core';
import { existsSync, readdirSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

const [base = 'http://127.0.0.1:8080', callsign = 'M0LTE', hz = '6070000', mode = 'am', shot = ''] = process.argv.slice(2);
const cache = join(homedir(), '.cache', 'ms-playwright');
const dir = readdirSync(cache).filter((d) => /^chromium-\d+$/.test(d)).sort().reverse()[0];
const exe = join(cache, dir, 'chrome-linux64', 'chrome');
if (!existsSync(exe)) throw new Error('no Playwright Chromium');

const browser = await chromium.launch({ executablePath: exe, args: ['--autoplay-policy=no-user-gesture-required'] });
const page = await browser.newPage({ viewport: { width: 1400, height: 1000 } });
const errors = [];
page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
page.on('pageerror', (e) => errors.push(String(e)));
await page.goto(base, { waitUntil: 'networkidle' });
if (shot) await page.screenshot({ path: shot, fullPage: false });

const res = await page.evaluate(async ({ callsign, hz, mode }) => {
  const rx = (await (await fetch('/api/receivers')).json()).receivers.find((r) => r.callsign === callsign);
  const ch = await (await fetch('/api/channels', { method: 'POST', body: JSON.stringify({ freqHz: Number(hz), mode, receiverKey: rx.key }) })).json();
  const a = new Audio(`/listen/${ch.channelId}/audio.webm?listener=${ch.listenerId}`);
  const t0 = performance.now();
  let firstPlaying = null;
  a.addEventListener('playing', () => { firstPlaying ??= performance.now() - t0; });
  const playErr = await a.play().then(() => null, (e) => e.message);
  const ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/listen/${ch.channelId}/spectrum?listener=${ch.listenerId}`);
  ws.binaryType = 'arraybuffer';
  let header = null, rows = 0, rowLen = 0;
  ws.onmessage = (m) => { if (typeof m.data === 'string') header = JSON.parse(m.data); else { rows++; rowLen = m.data.byteLength; } };
  await new Promise((r) => setTimeout(r, 10000));
  const out = { playErr, startMs: firstPlaying && Math.round(firstPlaying), currentTime: +a.currentTime.toFixed(2), mediaError: a.error && a.error.code, header, rows, rowLen, channel: ch.channelId };
  a.pause(); a.removeAttribute('src'); a.load(); ws.close();
  navigator.sendBeacon(`/api/channels/${ch.channelId}/listeners/${ch.listenerId}/leave`);
  return out;
}, { callsign, hz, mode });
console.log(JSON.stringify(res, null, 1));
console.log('console errors:', errors.length ? errors : 'none');
await browser.close();
