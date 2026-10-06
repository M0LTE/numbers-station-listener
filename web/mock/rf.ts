// Synthetic RF for the mock: waterfall rows for the spectrum socket and a
// recorded-spectrogram PNG for the history strip. Dev-only; never bundled.

import { deflateSync, crc32 } from "node:zlib";
import { WATERFALL_RGB } from "../src/palette.ts";

export const BINS = 512;
export const SPAN_HZ = 12000;
export const DB_MIN = -130;
export const DB_MAX = -40;

function gauss(): number {
  return (Math.random() + Math.random() + Math.random() - 1.5) * 1.6;
}

function toByte(db: number): number {
  return Math.max(0, Math.min(255, Math.round(((db - DB_MIN) / (DB_MAX - DB_MIN)) * 255)));
}

/** One row generator per socket; `t` is seconds since the socket opened. */
export class RowSource {
  private readonly binHz = SPAN_HZ / BINS;
  private readonly interferers: Array<{ off: number; db: number; width: number }>;
  constructor(
    private readonly kind: "voice" | "cw" | "fsk",
    seed: number,
  ) {
    const rnd = (k: number): number => {
      const x = Math.sin(seed * 9301 + k * 49297) * 233280;
      return x - Math.floor(x);
    };
    this.interferers = [
      { off: -4200 + rnd(1) * 800, db: -98, width: 1 },
      { off: 4400 + rnd(2) * 900, db: -104, width: 2 },
      { off: -2600 + rnd(3) * 300, db: -110, width: 1 },
    ];
  }

  row(t: number): Uint8Array {
    const out = new Uint8Array(BINS);
    const qsb = 4 * Math.sin(t * 0.6);
    // Groups of five syllables, then a pause; mirrors the audio rhythm.
    const cycle = t % 4.9;
    const inSyl = cycle % 0.68;
    const sylEnv = cycle < 3.4 && inSyl < 0.42 ? Math.sin((inSyl / 0.42) * Math.PI) : 0;
    const syllable = Math.floor(t / 0.68);
    const f0 = 108 + ((syllable * 37) % 25);
    const f1 = 450 + ((syllable * 131) % 350);
    const f2 = 1100 + ((syllable * 211) % 1100);
    const cwOn = Math.sin(t * 11) > 0.1;
    const fskMark = Math.floor(t * 45.45) % 2 === 0;
    for (let i = 0; i < BINS; i++) {
      const off = (i - BINS / 2) * this.binHz;
      let p = Math.pow(10, (-117 + gauss() * 2.4) / 10);
      const add = (db: number): void => {
        p += Math.pow(10, db / 10);
      };
      for (const f of this.interferers) {
        if (Math.abs(off - f.off) <= f.width * this.binHz) add(f.db + gauss());
      }
      // A slow wobbling data signal near the lower edge.
      const drift = -5200 + 40 * Math.sin(t / 9);
      if (Math.abs(off - drift) < 150) add(-103 + gauss() * 2);
      if (this.kind === "voice") {
        if (Math.abs(off) < this.binHz) add(-72 + qsb);
        if (sylEnv > 0.05 && off > 250 && off < 2900) {
          // Voiced speech: harmonics of f0 under two formant humps.
          const harm = Math.abs(((off / f0) % 1) - 0.5) > 0.32 ? 1 : 0.25;
          const shape = Math.exp(-((off - f1) ** 2) / 6e4) + 0.6 * Math.exp(-((off - f2) ** 2) / 1.5e5) + 0.15;
          add(-112 + 34 * shape * harm * sylEnv + qsb + gauss() * 2);
        }
      } else if (this.kind === "cw") {
        if (cwOn && Math.abs(off) < this.binHz * 1.5) add(-74 + qsb);
      } else {
        const tone = fskMark ? 85 : -85;
        if (Math.abs(off - tone) < this.binHz * 1.5) add(-78 + qsb);
      }
      out[i] = toByte(10 * Math.log10(p));
    }
    return out;
  }
}

function chunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const td = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td) >>> 0, 0);
  return Buffer.concat([len, td, crc]);
}

/**
 * A recorded-spectrogram PNG in UberSDR's orientation: one row per minute,
 * oldest at the top; columns run up in frequency, centred on the channel.
 * `startedAgoMin` is how long ago the transmission began (null if it has not).
 */
export function spectrogramPng(minutes: number, startedAgoMin: number | null, seed: number): Buffer {
  const width = 31;
  const height = minutes;
  const raw = Buffer.alloc((width * 3 + 1) * height);
  const rnd = (k: number): number => {
    const x = Math.sin(seed * 12.9898 + k * 78.233) * 43758.5453;
    return x - Math.floor(x);
  };
  for (let y = 0; y < height; y++) {
    const ago = height - 1 - y;
    raw[y * (width * 3 + 1)] = 0;
    for (let x = 0; x < width; x++) {
      let db = -112 + (rnd(y * 131 + x) - 0.5) * 8;
      if (startedAgoMin !== null && ago <= startedAgoMin) {
        if (x === 15) db = -76 + (rnd(y) - 0.5) * 6;
        else if (x === 16) db = -96 + (rnd(y + 7) - 0.5) * 6;
      }
      if (x === 4 && rnd(y * 3) > 0.3) db = -92;
      if (x === 23 && ago > 12 && ago < 22) db = -88 + (rnd(y * 5) - 0.5) * 4;
      const v = toByte(db);
      const o = y * (width * 3 + 1) + 1 + x * 3;
      raw[o] = WATERFALL_RGB[v * 3];
      raw[o + 1] = WATERFALL_RGB[v * 3 + 1];
      raw[o + 2] = WATERFALL_RGB[v * 3 + 2];
    }
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;
  ihdr[9] = 2;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw)),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}
