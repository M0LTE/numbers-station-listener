// Mock backend for `npm run dev`. Implements the docs/api.md routes the
// frontend uses, with switchable failure modes so every UI state can be
// seen. Dev server only (apply: "serve"); never part of the bundle.
//
// Switches are query parameters on the page URL, e.g.
//   http://localhost:5173/?audio=fail-first&spectrogram=404
// The page request copies them into a cookie, which the API, audio and
// WebSocket requests then carry. Loading the page without parameters resets
// everything to the happy path.
//
//   audio=ok|503|busy|502|fail-first|eof|eof-first
//       503/busy/502: every receiver fails; fail-first: only the best ranked
//       one does (shows the automatic fallback); eof: every stream ends after
//       about 16 s; eof-first: only the best one does.
//   spectrogram=404     history PNG missing (strip hides itself)
//   hist=0              capabilities.historicalSpectrogram false
//   spectrum=off|error  liveSpectrum false, or an error frame after 4 s
//   sse=off             /api/live answers 503 (page falls back to polling)
//   anchor=HH:MM        fixture time of day mapped onto now (default 11:04)
//
// GET /__mock/state lists open audio streams and spectrum sockets per
// channel, and the terminal logs every open, close and leave, so the idle
// connection policy can be checked by eye.

import type { Plugin } from "vite";
import type { IncomingMessage, ServerResponse } from "node:http";
import { randomBytes } from "node:crypto";
import { buildNow, rank, publicSummary, strip, RECEIVERS, type MockEvent, type RankedReceiver } from "./schedule.ts";
import { acceptUpgrade, type MockSocket } from "./ws.ts";
import { RATE, Synth, wavHeader } from "./audio.ts";
import { BINS, DB_MAX, DB_MIN, RowSource, SPAN_HZ, spectrogramPng } from "./rf.ts";

const COOKIE = "nslmock";
const SWITCHES = ["audio", "spectrogram", "spectrum", "sse", "hist", "anchor"] as const;
type Flags = Partial<Record<(typeof SWITCHES)[number], string>>;

function flagsOf(req: IncomingMessage): Flags {
  const c = req.headers.cookie ?? "";
  const m = new RegExp(`(?:^|;\\s*)${COOKIE}=([^;]*)`).exec(c);
  const out: Flags = {};
  if (!m) return out;
  const p = new URLSearchParams(decodeURIComponent(m[1]));
  for (const k of SWITCHES) {
    const v = p.get(k);
    if (v) out[k] = v;
  }
  return out;
}

interface Channel {
  id: string;
  eventId: string | null;
  freqHz: number;
  mode: string;
  digital: boolean;
  receiver: RankedReceiver;
  rank: number;
  startMs: number | null;
  audio: number;
  spectrum: number;
}

const channels = new Map<string, Channel>();

function log(msg: string): void {
  console.log(`[mock] ${msg}`);
}

function report(ch: Channel, why: string): void {
  const n = ch.audio + ch.spectrum;
  log(`channel ${ch.id.slice(0, 6)} ${ch.receiver.callsign} ${ch.freqHz / 1000} kHz: ${why}; listeners ${n} (audio ${ch.audio}, spectrum ${ch.spectrum})${n === 0 ? ", upstream would close after the 10 s grace" : ""}`);
}

function json(res: ServerResponse, status: number, body: unknown): void {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.setHeader("Cache-Control", "no-store");
  res.end(JSON.stringify(body));
}

function readBody(req: IncomingMessage): Promise<string> {
  return new Promise((resolve) => {
    let s = "";
    req.on("data", (d: Buffer) => (s += d.toString("utf8")));
    req.on("end", () => resolve(s));
  });
}

function nowPayload(flags: Flags): Record<string, unknown> {
  const t = Date.now();
  const s = buildNow(t, flags.anchor);
  const updated = new Date(Math.floor(t / 900_000) * 900_000).toISOString().replace(".000", "");
  return {
    serverTime: new Date(t).toISOString().replace(/\.\d+Z$/, "Z"),
    scheduleUpdated: updated,
    now: s.now.map(strip),
    next: s.next.map(strip),
    later: s.later.map(strip),
  };
}

function findEvent(id: string, flags: Flags): MockEvent | undefined {
  return buildNow(Date.now(), flags.anchor).all.find((e) => e.id === id);
}

function hash(s: string): string {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return (h >>> 0).toString(16).padStart(8, "0") + s.length.toString(16).padStart(8, "0");
}

function kindOf(ch: Channel): "voice" | "cw" | "fsk" {
  if (ch.digital) return "fsk";
  if (ch.mode === "cwu" || ch.mode === "cwl") return "cw";
  return "voice";
}

async function createChannel(req: IncomingMessage, res: ServerResponse, flags: Flags): Promise<void> {
  let body: { eventId?: string; freqHz?: number; mode?: string; receiverKey?: string };
  try {
    body = JSON.parse(await readBody(req));
  } catch {
    return json(res, 400, { error: "bad_body" });
  }
  let ranked: RankedReceiver[];
  let freqHz: number;
  let mode: string;
  let digital = false;
  let eventId: string | null = null;
  let startMs: number | null = null;
  if (body.eventId) {
    const ev = findEvent(body.eventId, flags);
    if (!ev || ev.search || !ev.parsed) return json(res, 404, { error: "no_event" });
    freqHz = body.freqHz ?? ev.freqs[0].hz;
    const r = ev._ranked.get(freqHz);
    if (!r) return json(res, 404, { error: "no_freq" });
    ranked = r;
    mode = ev.mode;
    digital = ev.digital;
    eventId = ev.id;
    startMs = Date.parse(ev.start);
  } else if (body.freqHz && body.mode) {
    freqHz = body.freqHz;
    mode = body.mode;
    ranked = rank(freqHz, mode, undefined);
  } else {
    return json(res, 400, { error: "bad_body" });
  }
  const idx = body.receiverKey ? ranked.findIndex((r) => r.key === body.receiverKey) : 0;
  if (idx < 0) {
    return json(res, RECEIVERS.some((r) => r.key === body.receiverKey) ? 409 : 404, { error: "no_receiver" });
  }
  const receiver = ranked[idx];
  const id = hash(`${receiver.key}|${freqHz}|${mode}`);
  let ch = channels.get(id);
  if (!ch) {
    ch = { id, eventId, freqHz, mode, digital, receiver, rank: idx, startMs, audio: 0, spectrum: 0 };
    channels.set(id, ch);
  }
  const listenerId = randomBytes(8).toString("hex");
  log(`POST /api/channels -> ${id.slice(0, 6)} on ${receiver.callsign} (rank ${idx + 1}), listener ${listenerId.slice(0, 6)}; nothing opened upstream`);
  json(res, 200, {
    channelId: id,
    listenerId,
    receiver: publicSummary(receiver),
    freqHz,
    mode,
    spanHz: SPAN_HZ,
    capabilities: { historicalSpectrogram: flags.hist !== "0", liveSpectrum: flags.spectrum !== "off" },
    alternatives: ranked.filter((r) => r.key !== receiver.key).slice(0, 5).map(publicSummary),
  });
}

function streamAudio(req: IncomingMessage, res: ServerResponse, ch: Channel, flags: Flags): void {
  const a = flags.audio ?? "ok";
  if (a === "503" || (a === "fail-first" && ch.rank === 0)) {
    log(`audio for ${ch.receiver.callsign}: 503 rejected (audio=${a})`);
    return json(res, 503, { error: "rejected", reason: "receiver full" });
  }
  if (a === "busy") return json(res, 503, { error: "receiver_busy" });
  if (a === "502") return json(res, 502, { error: "upstream", reason: "connection reset" });
  res.statusCode = 200;
  res.setHeader("Content-Type", "audio/wav");
  res.setHeader("Cache-Control", "no-store");
  ch.audio++;
  report(ch, `audio stream opened (range ${req.headers.range ?? "none"})`);
  const synth = new Synth(kindOf(ch));
  res.write(wavHeader());
  // Chrome buffers about 230 KB of a streaming WAV before it reports
  // metadata, so front-load ten seconds or the mock takes ages to start.
  res.write(synth.render(Math.round(RATE * 10)));
  let sent = 0;
  const t0 = Date.now();
  const eofAfter = a === "eof" || (a === "eof-first" && ch.rank === 0) ? 6000 : Infinity;
  const timer = setInterval(() => {
    if (res.writableEnded) return;
    const due = Math.round(((Date.now() - t0) / 1000) * RATE);
    if (due > sent) {
      res.write(synth.render(due - sent));
      sent = due;
    }
    if (Date.now() - t0 > eofAfter) {
      log(`audio for ${ch.receiver.callsign}: ending the stream (audio=${a})`);
      res.end();
    }
  }, 100);
  let closed = false;
  const done = (): void => {
    if (closed) return;
    closed = true;
    clearInterval(timer);
    ch.audio--;
    report(ch, "audio stream closed");
  };
  req.on("close", done);
  res.on("close", done);
}

function spectrumSocket(ws: MockSocket, ch: Channel, flags: Flags): void {
  ch.spectrum++;
  report(ch, "spectrum socket opened");
  const binHz = SPAN_HZ / BINS;
  ws.sendText(
    JSON.stringify({
      type: "header",
      startHz: ch.freqHz - SPAN_HZ / 2,
      binHz,
      bins: BINS,
      centerHz: ch.freqHz,
      tunedHz: ch.freqHz,
      dbMin: DB_MIN,
      dbMax: DB_MAX,
    }),
  );
  const src = new RowSource(kindOf(ch), ch.freqHz % 997);
  const t0 = Date.now();
  const rows = setInterval(() => {
    const t = (Date.now() - t0) / 1000;
    ws.sendBinary(src.row(t));
    if (flags.spectrum === "error" && t > 4) {
      ws.sendText(JSON.stringify({ type: "error", error: "upstream", reason: "spectrum feed lost" }));
      ws.close(1011, "upstream");
    }
  }, 100);
  const pings = setInterval(() => {
    if (Date.now() - ws.lastPong > 15_000) {
      log("spectrum socket: no pong for 15 s, dropping");
      ws.close(1001, "ping timeout");
      return;
    }
    ws.ping();
  }, 5000);
  ws.onClose = () => {
    clearInterval(rows);
    clearInterval(pings);
    ch.spectrum--;
    report(ch, "spectrum socket closed");
  };
}

function sse(req: IncomingMessage, res: ServerResponse, flags: Flags): void {
  if (flags.sse === "off") {
    log("SSE refused (sse=off); the page should fall back to polling /api/now");
    return json(res, 503, { error: "sse_disabled" });
  }
  res.statusCode = 200;
  res.setHeader("Content-Type", "text/event-stream");
  res.setHeader("Cache-Control", "no-store");
  res.setHeader("Connection", "keep-alive");
  const send = (): void => {
    res.write(`event: now\ndata: ${JSON.stringify(nowPayload(flags))}\n\n`);
  };
  send();
  const upd = setInterval(send, 30_000);
  const ping = setInterval(() => res.write(": ping\n\n"), 25_000);
  req.on("close", () => {
    clearInterval(upd);
    clearInterval(ping);
  });
}

export function mockBackend(): Plugin {
  return {
    name: "nsl-mock-backend",
    apply: "serve",
    configureServer(server) {
      server.httpServer?.on("upgrade", (req: IncomingMessage, socket, head: Buffer) => {
        const url = new URL(req.url ?? "/", "http://mock");
        const m = /^\/listen\/([^/]+)\/spectrum$/.exec(url.pathname);
        if (!m) return; // leave Vite's own HMR socket alone
        const ch = channels.get(m[1]);
        const ws = acceptUpgrade(req, socket, head);
        if (!ws) return;
        if (!ch) {
          ws.sendText(JSON.stringify({ type: "error", error: "no_channel" }));
          ws.close(1008, "no_channel");
          return;
        }
        spectrumSocket(ws, ch, flagsOf(req));
      });

      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://mock");
        const path = url.pathname;
        const flags = flagsOf(req);

        // Page load: remember this URL's switches for the requests that follow.
        if (req.method === "GET" && (path === "/" || path === "/index.html")) {
          const p = new URLSearchParams();
          for (const k of SWITCHES) {
            const v = url.searchParams.get(k);
            if (v) p.set(k, v);
          }
          res.setHeader("Set-Cookie", `${COOKIE}=${encodeURIComponent(p.toString())}; Path=/; SameSite=Lax`);
          if ([...p.keys()].length) log(`switches: ${p.toString()}`);
          return next();
        }

        if (path === "/api/now" && req.method === "GET") return json(res, 200, nowPayload(flags));
        if (path === "/api/live" && req.method === "GET") return sse(req, res, flags);
        if (path === "/api/channels" && req.method === "POST") {
          void createChannel(req, res, flags);
          return;
        }
        let m = /^\/api\/channels\/([^/]+)\/listeners\/([^/]+)(\/leave)?$/.exec(path);
        if (m && ((req.method === "DELETE" && !m[3]) || (req.method === "POST" && m[3]))) {
          log(`leave: channel ${m[1].slice(0, 6)} listener ${m[2].slice(0, 6)}`);
          res.statusCode = 204;
          return res.end();
        }
        m = /^\/listen\/([^/]+)\/audio\.webm$/.exec(path);
        if (m && req.method === "GET") {
          const ch = channels.get(m[1]);
          if (!ch) return json(res, 404, { error: "no_channel" });
          return streamAudio(req, res, ch, flags);
        }
        m = /^\/listen\/([^/]+)\/spectrogram\.png$/.exec(path);
        if (m && req.method === "GET") {
          const ch = channels.get(m[1]);
          if (!ch || flags.spectrogram === "404" || flags.hist === "0") return json(res, 404, { error: "no_history" });
          const minutes = Math.max(5, Math.min(60, parseInt(url.searchParams.get("minutes") ?? "30", 10) || 30));
          const ago = ch.startMs !== null && ch.startMs < Date.now() ? Math.floor((Date.now() - ch.startMs) / 60_000) : null;
          res.statusCode = 200;
          res.setHeader("Content-Type", "image/png");
          res.setHeader("Cache-Control", "max-age=60");
          return res.end(spectrogramPng(minutes, ago, ch.freqHz % 1013));
        }
        if (path === "/__mock/state") {
          return json(res, 200, {
            channels: [...channels.values()].map((c) => ({ id: c.id, receiver: c.receiver.callsign, freqHz: c.freqHz, audio: c.audio, spectrum: c.spectrum })),
          });
        }
        if (path.startsWith("/api/") || path.startsWith("/listen/")) return json(res, 404, { error: "not_found" });
        next();
      });
    },
  };
}
