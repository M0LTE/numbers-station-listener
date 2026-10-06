// Synthetic receiver audio for the mock: an endless 16-bit mono WAV paced in
// real time. Voice modes get vowel-like syllables in groups of five over
// band-limited hiss; CW gets a keyed 700 Hz tone; digital modes get 170 Hz
// shift FSK at 45.45 baud. The real backend serves WebM/Opus; the <audio>
// element sniffs the content, so the mock can stay dependency-free.
// Dev-only; never bundled.

export const RATE = 12000;

export function wavHeader(): Buffer {
  const h = Buffer.alloc(44);
  h.write("RIFF", 0);
  h.writeUInt32LE(0xffffffff, 4); // unknown length: a live stream
  h.write("WAVE", 8);
  h.write("fmt ", 12);
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20);
  h.writeUInt16LE(1, 22);
  h.writeUInt32LE(RATE, 24);
  h.writeUInt32LE(RATE * 2, 28);
  h.writeUInt16LE(2, 32);
  h.writeUInt16LE(16, 34);
  h.write("data", 36);
  h.writeUInt32LE(0xffffffff - 36, 40);
  return h;
}

const VOWELS: Array<[number, number, number]> = [
  [700, 1220, 2600],
  [530, 1840, 2480],
  [300, 2200, 2950],
  [570, 840, 2410],
  [320, 900, 2240],
  [640, 1190, 2390],
];

class Resonator {
  private y1 = 0;
  private y2 = 0;
  private a1 = 0;
  private a2 = 0;
  private g = 0;
  set(freq: number, bw: number): void {
    const r = Math.exp((-Math.PI * bw) / RATE);
    this.a1 = 2 * r * Math.cos((2 * Math.PI * freq) / RATE);
    this.a2 = -r * r;
    this.g = 1 - r;
  }
  step(x: number): number {
    const y = this.g * x + this.a1 * this.y1 + this.a2 * this.y2;
    this.y2 = this.y1;
    this.y1 = y;
    return y;
  }
}

// International Morse for digits.
const MORSE = ["-----", ".----", "..---", "...--", "....-", ".....", "-....", "--...", "---..", "----."];

export class Synth {
  private n = 0;
  private phase = 0;
  private noiseLp = 0;
  private f = [new Resonator(), new Resonator(), new Resonator()];
  private sylEnd = 0;
  private sylLen = 0;
  private f0 = 120;
  private gapUntil = 0;
  private inGroup = 0;
  private keying: boolean[] = [];
  private bit = 0;

  constructor(private readonly kind: "voice" | "cw" | "fsk") {}

  private nextSyllable(): void {
    if (this.inGroup >= 5) {
      this.inGroup = 0;
      this.gapUntil = this.n + Math.round(RATE * 1.3);
      return;
    }
    this.inGroup++;
    const v = VOWELS[Math.floor(Math.random() * VOWELS.length)];
    this.f[0].set(v[0], 90);
    this.f[1].set(v[1], 120);
    this.f[2].set(v[2], 170);
    this.f0 = 105 + Math.random() * 30;
    this.sylLen = Math.round(RATE * (0.28 + Math.random() * 0.12));
    this.sylEnd = this.n + this.sylLen;
    this.gapUntil = this.sylEnd + Math.round(RATE * 0.22);
  }

  private nextMorse(): void {
    // 18 wpm: one dot = 66 ms. Five-figure groups, word gap between.
    const dot = Math.round(RATE * 0.066);
    const seq: boolean[] = [];
    for (let g = 0; g < 5; g++) {
      for (const c of MORSE[Math.floor(Math.random() * 10)]) {
        const on = c === "." ? 1 : 3;
        for (let i = 0; i < on * dot; i++) seq.push(true);
        for (let i = 0; i < dot; i++) seq.push(false);
      }
      for (let i = 0; i < 2 * dot; i++) seq.push(false);
    }
    for (let i = 0; i < 4 * dot; i++) seq.push(false);
    this.keying = seq;
  }

  render(count: number): Buffer {
    const out = Buffer.alloc(count * 2);
    for (let i = 0; i < count; i++, this.n++) {
      // Hiss: white noise lightly low-passed, with slow fading (QSB).
      const w = Math.random() * 2 - 1;
      this.noiseLp += 0.35 * (w - this.noiseLp);
      const qsb = 0.75 + 0.25 * Math.sin((this.n / RATE) * 0.6);
      let s = this.noiseLp * 0.06;

      if (this.kind === "voice") {
        if (this.n >= this.gapUntil && this.n >= this.sylEnd) this.nextSyllable();
        if (this.n < this.sylEnd) {
          const pos = 1 - (this.sylEnd - this.n) / this.sylLen;
          const env = Math.min(1, pos * 12) * Math.min(1, (1 - pos) * 5);
          this.phase += this.f0 / RATE;
          if (this.phase >= 1) this.phase -= 1;
          const pulse = this.phase < 0.08 ? 1 : 0;
          const x = pulse + w * 0.05;
          const v = this.f[0].step(x) * 1.0 + this.f[1].step(x) * 0.6 + this.f[2].step(x) * 0.3;
          s += v * env * 0.9 * qsb;
        }
      } else if (this.kind === "cw") {
        if (this.keying.length === 0) this.nextMorse();
        const on = this.keying.shift() ?? false;
        this.phase += 700 / RATE;
        s += on ? Math.sin(2 * Math.PI * this.phase) * 0.35 * qsb : 0;
      } else {
        const baud = Math.round(RATE / 45.45);
        if (this.n % baud === 0) this.bit = Math.random() < 0.5 ? 0 : 1;
        this.phase += (this.bit ? 2295 : 2125) / RATE;
        s += Math.sin(2 * Math.PI * this.phase) * 0.3 * qsb;
      }
      if (this.phase > 1e6) this.phase -= Math.floor(this.phase);
      out.writeInt16LE(Math.max(-32767, Math.min(32767, Math.round(s * 32767))), i * 2);
    }
    return out;
  }
}
