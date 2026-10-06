// Builds a realistic /api/now from the captured Priyom day
// (tests/fixtures/priyom-2026-10-06.json) and the station catalogue
// (data/stations.json), shifted so the fixture's busy late morning lines up
// with the current time. Dev-only; never bundled.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const repo = (p: string): string => fileURLToPath(new URL(`../../${p}`, import.meta.url));

interface CatalogueEntry {
  name: string;
  priyomUrl: string;
  category: string;
  language: string;
  txSite: { lat: number | null; lon: number | null; label: string };
  typicalDurationMin: number | null;
}

interface FeedItem {
  summary: string;
  start: { dateTime: string };
}

export interface MockReceiver {
  key: string;
  provider: string;
  callsign: string;
  name: string;
  location: string;
  country: string;
  lat: number;
  lon: number;
  publicUrl: string;
  availableClients: number;
  maxClients: number;
  snr: number;
}

// M0LTE is Tom's real instance (the project's dev target). The rest use
// placeholder callsigns (the amateur "0CALL" convention) and example.org
// URLs so nobody mistakes them for real stations.
export const RECEIVERS: MockReceiver[] = [
  r("b838fc45", "M0LTE", "SDR with Active Loop", "Reading, England, UK", "gb", 51.46, -0.98, "https://reading-ubersdr.m0lte.uk/", 18, 20, 21),
  r("1c0e7a21", "G0CALL", "Wellbrook loop, rural site", "Lanark, Scotland, UK", "gb", 55.67, -3.78, "https://g0call.example.org/", 6, 10, 24),
  r("5a91d3be", "PA0CALL", "Mini-Whip on a farm", "Enschede, Netherlands", "nl", 52.22, 6.89, "https://pa0call.example.org/", 11, 16, 17),
  r("77f2c0d4", "OE0CALL", "Delta loop, 20 m up", "Vienna, Austria", "at", 48.21, 16.37, "https://oe0call.example.org/", 3, 8, 19),
  r("9e40b612", "EA0CALL", "End-fed wire", "Valencia, Spain", "es", 39.47, -0.38, "https://ea0call.example.org/", 9, 12, 15),
  r("3d58aa07", "N0CALL", "Beverage array", "Lawrence, Kansas, US", "us", 38.97, -95.24, "https://n0call.example.org/", 14, 16, 22),
  r("c2b7e930", "VK0CALL", "Vertical on the coast", "Hobart, Tasmania, AU", "au", -42.88, 147.33, "https://vk0call.example.org/", 4, 6, 18),
];

function r(id: string, callsign: string, name: string, location: string, country: string, lat: number, lon: number, publicUrl: string, avail: number, max: number, snr: number): MockReceiver {
  return { key: `ubersdr:${id}-mock`, provider: "ubersdr", callsign, name, location, country, lat, lon, publicUrl, availableClients: avail, maxClients: max, snr };
}

const catalogue = JSON.parse(readFileSync(repo("data/stations.json"), "utf8")) as Record<string, CatalogueEntry>;
const feed = (JSON.parse(readFileSync(repo("tests/fixtures/priyom-2026-10-06.json"), "utf8")) as { items: FeedItem[] }).items;

// Mock-only additions so every UI state appears on screen: a
// multi-frequency entry, a Search entry soon, and a line the parser cannot
// read. Times are on the fixture's day.
const SYNTHETIC: FeedItem[] = [
  { summary: "V13 9276, 11430kHz USB/AM [Target: East Asia]", start: { dateTime: "2026-10-06T11:30:00.000Z" } },
  { summary: "F06 Search [Last used: 11405kHz]", start: { dateTime: "2026-10-06T11:25:00.000Z" } },
  { summary: "S28 4625kHz [buzzer, voice message expected]", start: { dateTime: "2026-10-06T11:50:00.000Z" } },
];

const DIGITAL = /^(RTTY|FSK|MFSK|PSK|OFDM|FAX|BPSK|QPSK|MT63|OLIVIA)/;

function mapMode(m: string): { mode: string; digital: boolean } {
  if (m === "USB" || m === "USB/AM") return { mode: "usb", digital: false };
  if (m === "LSB") return { mode: "lsb", digital: false };
  if (m === "AM" || m === "MCW") return { mode: "am", digital: false };
  if (m === "CW") return { mode: "cwu", digital: false };
  if (DIGITAL.test(m)) return { mode: "usb", digital: true };
  return { mode: "usb", digital: false };
}

function fnv(s: string): string {
  let h = 0xcbf29ce484222325n;
  for (let i = 0; i < s.length; i++) {
    h ^= BigInt(s.charCodeAt(i));
    h = (h * 0x100000001b3n) & 0xffffffffffffffffn;
  }
  return h.toString(16).padStart(16, "0");
}

function rand01(seed: string): number {
  // FNV's high bits mix poorly for near-identical strings; finish with a
  // murmur3-style avalanche on the low word.
  let x = parseInt(fnv(seed).slice(8), 16) >>> 0;
  x = Math.imul(x ^ (x >>> 16), 0x85ebca6b) >>> 0;
  x = Math.imul(x ^ (x >>> 13), 0xc2b2ae35) >>> 0;
  x = (x ^ (x >>> 16)) >>> 0;
  return x / 0xffffffff;
}

function lookup(station: string): CatalogueEntry | undefined {
  return catalogue[station] ?? catalogue[station.replace(/[a-z]+$/, "")];
}

function distKm(aLat: number, aLon: number, bLat: number, bLon: number): number {
  const rad = Math.PI / 180;
  const dLat = (bLat - aLat) * rad;
  const dLon = (bLon - aLon) * rad;
  const x = Math.sin(dLat / 2) ** 2 + Math.cos(aLat * rad) * Math.cos(bLat * rad) * Math.sin(dLon / 2) ** 2;
  return 6371 * 2 * Math.asin(Math.sqrt(x));
}

export interface RankedReceiver extends MockReceiver {
  distanceKm: number | null;
  score: number;
  reasons: string[];
  deepLink: string;
}

export function rank(hz: number, mode: string, tx: CatalogueEntry["txSite"] | undefined): RankedReceiver[] {
  const out = RECEIVERS.map((rx): RankedReceiver => {
    const reasons: string[] = [];
    let score = 0.3 + (rx.snr - 15) / 90;
    let distanceKm: number | null = null;
    if (tx && tx.lat !== null && tx.lon !== null) {
      distanceKm = Math.round(distKm(tx.lat, tx.lon, rx.lat, rx.lon));
      const lo = hz < 10e6 ? 300 : 1500;
      const hi = hz < 10e6 ? 2500 : 6000;
      if (distanceKm >= lo && distanceKm <= hi) {
        score += 0.35;
        reasons.push(hz < 10e6 ? "night path, 1-hop F2" : "daylight path, 1-hop F2");
      } else if (distanceKm < lo) {
        score -= 0.15;
        reasons.push("likely in the skip zone");
      } else {
        score += 0.1;
        reasons.push("multi-hop path");
      }
    }
    if (rx.snr >= 20) reasons.push("good receiver SNR");
    reasons.push(`${rx.availableClients} free slots`);
    score += rx.availableClients / rx.maxClients / 5;
    const deepLink = `${rx.publicUrl}?freq=${hz}&mode=${mode}`;
    return { ...rx, distanceKm, score: Math.max(0, Math.min(1, Math.round(score * 100) / 100)), reasons, deepLink };
  });
  return out.sort((a, b) => b.score - a.score);
}

export function publicSummary(rx: RankedReceiver): Record<string, unknown> {
  const { snr: _snr, ...rest } = rx;
  void _snr;
  return rest;
}

const PARSE = /^(\w+)\s+(?:(Search)|((?:\d+(?:\.\d+)?\s*(?:kHz)?\s*,\s*)*\d+(?:\.\d+)?\s*kHz)\s+([A-Z][A-Z0-9/]*))\s*(.*)$/;

export interface MockEvent {
  id: string;
  station: string;
  stationName: string;
  priyomUrl: string | null;
  language: string | null;
  category: string | null;
  start: string;
  end: string;
  endEstimated: boolean;
  status: "upcoming" | "live" | "done";
  search: boolean;
  freqs: Array<{ hz: number; signal: Record<string, unknown>; receivers: Array<Record<string, unknown>> }>;
  priyomMode: string;
  mode: string;
  digital: boolean;
  remarks: string[];
  target: string | null;
  raw: string;
  parsed: boolean;
  // mock-only bookkeeping, stripped before sending
  _ranked: Map<number, RankedReceiver[]>;
}

function build(item: FeedItem, startMs: number, nowMs: number): MockEvent {
  const raw = item.summary;
  const m = PARSE.exec(raw);
  const station = raw.split(/\s+/)[0] ?? raw;
  const cat = lookup(station);
  const durMin = cat?.typicalDurationMin ?? 10;
  const endMs = startMs + durMin * 60_000;
  const status = nowMs >= endMs ? "done" : nowMs >= startMs ? "live" : "upcoming";
  const base = {
    station,
    stationName: cat?.name ?? "",
    priyomUrl: cat?.priyomUrl ?? null,
    language: cat?.language ?? null,
    category: cat?.category ?? null,
    start: new Date(startMs).toISOString().replace(".000", ""),
    end: new Date(endMs).toISOString().replace(".000", ""),
    endEstimated: true,
    status: status as MockEvent["status"],
    raw,
    _ranked: new Map<number, RankedReceiver[]>(),
  };
  if (!m) {
    return { ...base, id: fnv(`${startMs}|${raw}`), search: false, freqs: [], priyomMode: "", mode: "usb", digital: false, remarks: [], target: null, parsed: false };
  }
  const rest = m[5] ?? "";
  const remarks: string[] = [];
  for (const x of rest.matchAll(/\[([^\]]+)\]|\(([^)]+)\)/g)) remarks.push((x[1] ?? x[2]).trim());
  const loose = rest.replace(/\[[^\]]*\]|\([^)]*\)/g, "").trim();
  if (loose) remarks.push(loose);
  const target = remarks.find((x) => /^Target:/i.test(x))?.replace(/^Target:\s*/i, "") ?? null;
  const search = m[2] === "Search";
  const priyomMode = m[4] ?? "";
  const { mode, digital } = mapMode(priyomMode);
  const hzs = search ? [] : (m[3] ?? "").replace(/kHz/g, "").split(",").map((s) => Math.round(parseFloat(s) * 1000));
  const freqs = hzs.map((hz) => {
    const ranked = rank(hz, mode, cat?.txSite);
    base._ranked.set(hz, ranked);
    let signal: Record<string, unknown> = { state: "unknown" };
    if (status === "live" || (status === "upcoming" && startMs - nowMs < 60_000)) {
      const p = rand01(`${startMs}${hz}`);
      const at = new Date(nowMs - Math.floor(p * 50_000)).toISOString().replace(/\.\d+Z$/, "Z");
      if (status === "live" && p < 0.62) signal = { state: "present", snr: Math.round((6 + p * 26) * 10) / 10, at, receiverKey: ranked[0].key };
      else if (status === "live" && p < 0.86) signal = { state: "absent", snr: Math.round(p * 30) / 10, at, receiverKey: ranked[0].key };
    }
    return { hz, signal, receivers: ranked.slice(0, 3).map(publicSummary) };
  });
  return {
    ...base,
    id: fnv(`${startMs}|${station}|${hzs.join(",")}`),
    search,
    freqs,
    priyomMode,
    mode,
    digital,
    remarks: remarks.filter((x) => !/^Target:/i.test(x)),
    target,
    parsed: true,
  };
}

const shifts = new Map<string, number>();

// Fixed once per anchor for the life of the dev server, so the schedule
// then runs in real time (countdowns reach zero, events go live) and event
// ids stay stable. Whole 5-minute steps keep start times on round figures.
function shiftFor(anchor: string, nowMs: number): number {
  let s = shifts.get(anchor);
  if (s === undefined) {
    const [ah, am] = anchor.split(":").map((x) => parseInt(x, 10));
    const anchorMs = Date.UTC(2026, 9, 6, ah || 0, am || 0);
    s = Math.floor((nowMs - anchorMs) / 300_000) * 300_000;
    shifts.set(anchor, s);
  }
  return s;
}

/** Fixture time of day (UTC, "HH:MM") that is mapped onto "now". */
export function buildNow(nowMs: number, anchor = "11:04"): { now: MockEvent[]; next: MockEvent[]; later: MockEvent[]; all: MockEvent[] } {
  const shift = shiftFor(anchor, nowMs);
  const all: MockEvent[] = [];
  for (const day of [-1, 0, 1]) {
    for (const item of [...feed, ...SYNTHETIC]) {
      const start = Date.parse(item.start.dateTime) + shift + day * 86_400_000;
      if (start < nowMs - 3 * 3_600_000 || start > nowMs + 24 * 3_600_000) continue;
      all.push(build(item, start, nowMs));
    }
  }
  all.sort((a, b) => Date.parse(a.start) - Date.parse(b.start) || a.station.localeCompare(b.station));
  const now = all.filter((e) => e.status === "live").sort((a, b) => Date.parse(a.end) - Date.parse(b.end));
  const upcoming = all.filter((e) => e.status === "upcoming");
  return { now, next: upcoming.slice(0, 8), later: upcoming.slice(8), all };
}

export function strip(e: MockEvent): Omit<MockEvent, "_ranked"> {
  const { _ranked, ...rest } = e;
  void _ranked;
  return rest;
}
