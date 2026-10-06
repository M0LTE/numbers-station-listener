// A live WebM/Opus stream for the mock, shaped like the real relay's output
// (internal/webm): EBML header, unknown-size Segment, Info, one Opus track,
// then a known-size Cluster every 200 ms, timestamps from zero, paced in
// real time with no backlog. The Opus packets are the 5 s of real receiver
// audio captured from M0LTE in tests/fixtures/ubersdr (UberSDR /ws version 4
// frames), looped. Dev-only; never bundled.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const FIXTURE = fileURLToPath(new URL("../../tests/fixtures/ubersdr/audio-opus-v4-7910k-usb.rec", import.meta.url));

/** Strips the version 4 frame header (docs/ubersdr-protocol.md) and returns the Opus packet. */
function stripV4(b: Buffer): Buffer | null {
  const flags = b[0];
  if (flags > 3) return null;
  let off = 1;
  const skipVarint = (): void => {
    while (off < b.length && b[off] & 0x80) off++;
    off++;
  };
  if (flags & 2) {
    off += 8; // absolute timestamp, ns
    skipVarint(); // sample rate
    off += 1; // channels
  } else {
    skipVarint(); // zigzag delta timestamp
  }
  if (flags & 1) off += 4; // power and noise, centi-dB
  return off < b.length ? b.subarray(off) : null;
}

function loadPackets(): Buffer[] {
  const rec = readFileSync(FIXTURE);
  const out: Buffer[] = [];
  for (let o = 0; o + 9 <= rec.length; ) {
    const kind = rec[o];
    const len = rec.readUInt32LE(o + 5);
    const payload = rec.subarray(o + 9, o + 9 + len);
    o += 9 + len;
    if (kind !== 0) continue;
    const p = stripV4(payload);
    if (p) out.push(p);
  }
  // Keep single-frame 20 ms packets (TOC code 0, configs 1, 5, 9, 13, 15,
  // 19, 23, 27, 31); the capture is all SILK mediumband, config 5.
  const twentyMs = new Set([1, 5, 9, 13, 15, 19, 23, 27, 31]);
  const ok = out.filter((p) => twentyMs.has(p[0] >> 3) && (p[0] & 3) === 0);
  if (ok.length === 0) throw new Error(`mock: no usable Opus packets in ${FIXTURE}`);
  return ok;
}

export const PACKETS = loadPackets();
const PACKET_MS = 20;

function idBytes(id: number): Buffer {
  const out: number[] = [];
  for (let v = id; v > 0; v = Math.floor(v / 256)) out.unshift(v & 0xff);
  return Buffer.from(out);
}

function size(n: number): Buffer {
  if (n < 0x7f) return Buffer.from([0x80 | n]);
  if (n < 0x3fff) return Buffer.from([0x40 | (n >> 8), n & 0xff]);
  if (n < 0x1fffff) return Buffer.from([0x20 | (n >> 16), (n >> 8) & 0xff, n & 0xff]);
  return Buffer.from([0x10 | (n >>> 24), (n >> 16) & 0xff, (n >> 8) & 0xff, n & 0xff]);
}

function el(id: number, ...parts: Buffer[]): Buffer {
  const body = Buffer.concat(parts);
  return Buffer.concat([idBytes(id), size(body.length), body]);
}

function uint(id: number, v: number): Buffer {
  const b: number[] = [];
  for (let x = v; x > 0; x = Math.floor(x / 256)) b.unshift(x & 0xff);
  return el(id, Buffer.from(b.length ? b : [0]));
}

function str(id: number, s: string): Buffer {
  return el(id, Buffer.from(s, "utf8"));
}

function float(id: number, v: number): Buffer {
  const b = Buffer.alloc(8);
  b.writeDoubleBE(v, 0);
  return el(id, b);
}

function opusHead(): Buffer {
  const b = Buffer.alloc(19);
  b.write("OpusHead", 0, "ascii");
  b[8] = 1; // version
  b[9] = 1; // mono
  b.writeUInt16LE(0, 10); // pre-skip
  b.writeUInt32LE(12000, 12); // input sample rate (informational)
  b.writeUInt16LE(0, 16); // gain
  b[18] = 0; // mapping family
  return b;
}

export function webmHeader(): Buffer {
  const ebml = el(0x1a45dfa3, uint(0x4286, 1), uint(0x42f7, 1), uint(0x42f2, 4), uint(0x42f3, 8), str(0x4282, "webm"), uint(0x4287, 4), uint(0x4285, 2));
  const info = el(0x1549a966, uint(0x2ad7b1, 1_000_000), str(0x4d80, "nsl-mock"), str(0x5741, "nsl-mock"));
  const track = el(
    0xae,
    uint(0xd7, 1),
    uint(0x73c5, 1),
    uint(0x83, 2),
    uint(0x9c, 0),
    str(0x86, "A_OPUS"),
    el(0x63a2, opusHead()),
    uint(0x56aa, 0),
    uint(0x56bb, 80_000_000),
    el(0xe1, float(0xb5, 48000), uint(0x9f, 1)),
  );
  const segmentStart = Buffer.concat([idBytes(0x18538067), Buffer.from([0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff])]);
  return Buffer.concat([ebml, segmentStart, info, el(0x1654ae6b, track)]);
}

/** Cluster of `count` packets starting at packet index `first` (looping), timecode in ms. */
export function cluster(first: number, count: number, timecodeMs: number): Buffer {
  const blocks: Buffer[] = [uint(0xe7, timecodeMs)];
  for (let i = 0; i < count; i++) {
    const p = PACKETS[(first + i) % PACKETS.length];
    const rel = i * PACKET_MS;
    blocks.push(el(0xa3, Buffer.from([0x81, (rel >> 8) & 0xff, rel & 0xff, 0x80]), p));
  }
  return el(0x1f43b675, ...blocks);
}

export const PACKETS_PER_CLUSTER = 10; // 200 ms, like the relay's default
export const CLUSTER_MS = PACKETS_PER_CLUSTER * PACKET_MS;
