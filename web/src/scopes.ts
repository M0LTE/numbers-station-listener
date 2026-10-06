// The three pictures in the player: the RF waterfall fed by the spectrum
// socket, the audio spectrogram fed by an AnalyserNode, and the recorded
// history strip.

import { WATERFALL_LUT } from "./palette";
import type { SpectrumHeader } from "./types";
import { h } from "./dom";
import { kHz } from "./format";
import { isNarrow } from "./layout";

/** One waterfall row per CSS pixel of height: 220 on a desktop, 140 on a phone. */
const rfRows = (): number => (isNarrow() ? 140 : 220);

export class RfWaterfall {
  private ctx: CanvasRenderingContext2D;
  private header: SpectrumHeader | null = null;
  private row: ImageData | null = null;
  private row32: Uint32Array | null = null;
  private mode = "";
  private stationHz = 0;

  constructor(
    private readonly canvas: HTMLCanvasElement,
    private readonly marker: HTMLElement,
    private readonly scale: HTMLElement,
    private readonly pass: HTMLElement,
  ) {
    const ctx = canvas.getContext("2d", { alpha: false });
    if (!ctx) throw new Error("no 2d context");
    this.ctx = ctx;
    if (typeof ResizeObserver === "function") {
      new ResizeObserver(() => {
        if (this.header) this.layout(this.header);
      }).observe(scale);
    }
  }

  clear(): void {
    this.header = null;
    this.ctx.fillStyle = "#0b0405";
    this.ctx.fillRect(0, 0, this.canvas.width, this.canvas.height);
    this.scale.replaceChildren();
    this.marker.hidden = true;
    this.pass.hidden = true;
  }

  /**
   * stationHz is the station's listed frequency, where the red marker goes.
   * The header's tunedHz is the receiver's dial, which the passband hangs off;
   * for data modes the dial sits 1500 Hz below the listed frequency.
   */
  setHeader(hd: SpectrumHeader, mode: string, stationHz: number): void {
    const geometryChanged = !this.header || this.header.bins !== hd.bins || this.header.startHz !== hd.startHz || this.header.binHz !== hd.binHz;
    this.header = hd;
    this.mode = mode;
    this.stationHz = stationHz || hd.tunedHz;
    if (geometryChanged) {
      this.canvas.width = hd.bins;
      this.canvas.height = rfRows();
      this.ctx.fillStyle = "#0b0405";
      this.ctx.fillRect(0, 0, hd.bins, rfRows());
      this.row = this.ctx.createImageData(hd.bins, 1);
      this.row32 = new Uint32Array(this.row.data.buffer);
    }
    this.layout(hd);
  }

  private layout(hd: SpectrumHeader): void {
    const mode = this.mode;
    const spanHz = hd.bins * hd.binHz;
    const pct = (hz: number): number => ((hz - hd.startHz) / spanHz) * 100;

    this.marker.hidden = false;
    this.marker.style.left = `${pct(this.stationHz)}%`;

    // Receive passband for the mode relative to the dial, drawn on the scale.
    const pb: Record<string, [number, number]> = {
      usb: [300, 2700],
      lsb: [-2700, -300],
      am: [-4500, 4500],
      sam: [-4500, 4500],
      cwu: [-450, 450],
      cwl: [-450, 450],
    };
    const band = pb[mode];
    if (band) {
      const a = Math.max(0, pct(hd.tunedHz + band[0]));
      const b = Math.min(100, pct(hd.tunedHz + band[1]));
      this.pass.hidden = false;
      this.pass.style.left = `${a}%`;
      this.pass.style.width = `${Math.max(0, b - a)}%`;
    } else {
      this.pass.hidden = true;
    }

    // One label per 64 px or so: twelve on a desktop, five on a phone.
    const maxLabels = Math.max(3, Math.min(12, Math.floor((this.scale.clientWidth || 768) / 56)));
    const steps = [100, 200, 500, 1000, 2000, 5000, 10000, 20000, 50000];
    const step = steps.find((s) => spanHz / s <= maxLabels) ?? 100000;
    const ticks: HTMLElement[] = [];
    const first = Math.ceil(hd.startHz / step) * step;
    for (let f = first; f <= hd.startHz + spanHz; f += step) {
      const p = pct(f);
      if (p < 3 || p > 97) continue;
      ticks.push(h("span", { class: "tick", style: `left:${p}%` }, kHz(f)));
    }
    this.scale.replaceChildren(...ticks);
  }

  push(bytes: Uint8Array): void {
    const hd = this.header;
    if (!hd || !this.row || !this.row32 || bytes.length !== hd.bins) return;
    const { width, height } = this.canvas;
    this.ctx.drawImage(this.canvas, 0, 0, width, height - 1, 0, 1, width, height - 1);
    for (let i = 0; i < bytes.length; i++) this.row32[i] = WATERFALL_LUT[bytes[i]];
    this.ctx.putImageData(this.row, 0, 0);
  }
}

const AF_COLS = 480;
const AF_MAX_HZ = 4000;

export class AudioScope {
  private ctx: CanvasRenderingContext2D;
  private analyser: AnalyserNode | null = null;
  private data: Uint8Array<ArrayBuffer> = new Uint8Array(0);
  private col: ImageData | null = null;
  private col32: Uint32Array | null = null;
  private raf = 0;
  private last = 0;
  private bins = 0;

  constructor(private readonly canvas: HTMLCanvasElement) {
    const ctx = canvas.getContext("2d", { alpha: false });
    if (!ctx) throw new Error("no 2d context");
    this.ctx = ctx;
    this.clear();
  }

  clear(): void {
    this.canvas.width = AF_COLS;
    this.canvas.height = this.bins || 170;
    this.ctx.fillStyle = "#0b0405";
    this.ctx.fillRect(0, 0, this.canvas.width, this.canvas.height);
  }

  attach(analyser: AnalyserNode): void {
    this.analyser = analyser;
    const binHz = analyser.context.sampleRate / analyser.fftSize;
    this.bins = Math.min(analyser.frequencyBinCount, Math.ceil(AF_MAX_HZ / binHz));
    this.data = new Uint8Array(analyser.frequencyBinCount);
    this.canvas.height = this.bins;
    this.clear();
    this.col = this.ctx.createImageData(1, this.bins);
    this.col32 = new Uint32Array(this.col.data.buffer);
  }

  start(): void {
    if (this.raf || !this.analyser) return;
    const tick = (t: number): void => {
      this.raf = requestAnimationFrame(tick);
      // About 25 columns a second, so the canvas holds roughly 20 s.
      if (t - this.last < 40) return;
      this.last = t;
      this.draw();
    };
    this.raf = requestAnimationFrame(tick);
  }

  stop(): void {
    if (this.raf) cancelAnimationFrame(this.raf);
    this.raf = 0;
  }

  private draw(): void {
    const a = this.analyser;
    if (!a || !this.col || !this.col32) return;
    a.getByteFrequencyData(this.data);
    const w = this.canvas.width;
    const hgt = this.canvas.height;
    this.ctx.drawImage(this.canvas, 1, 0, w - 1, hgt, 0, 0, w - 1, hgt);
    for (let i = 0; i < this.bins; i++) this.col32[hgt - 1 - i] = WATERFALL_LUT[this.data[i]];
    this.ctx.putImageData(this.col, w - 1, 0);
  }
}

// UberSDR draws its recorded spectrogram in the rainbow "jet" map. Jet is
// not perceptually ordered, and it clashes with the waterfalls, so map each
// pixel back to its level and recolour it with the page's own map. Only done
// when the picture really is jet; anything else is shown as it came.
const JET: Array<[number, number, number]> = Array.from({ length: 256 }, (_, i) => {
  const t = i / 255;
  const c = (x: number): number => Math.round(255 * Math.max(0, Math.min(1, 1.5 - Math.abs(4 * t - x))));
  return [c(3), c(2), c(1)];
});

function recolourJet(ctx: CanvasRenderingContext2D, w: number, hgt: number): void {
  const img = ctx.getImageData(0, 0, w, hgt);
  const d = img.data;
  const levels = new Uint8Array(w * hgt);
  let err = 0;
  for (let p = 0, i = 0; i < d.length; i += 4, p++) {
    let best = 0;
    let bestD = Infinity;
    for (let k = 0; k < 256; k++) {
      const [r, g, b] = JET[k];
      const dist = (d[i] - r) ** 2 + (d[i + 1] - g) ** 2 + (d[i + 2] - b) ** 2;
      if (dist < bestD) {
        bestD = dist;
        best = k;
      }
    }
    levels[p] = best;
    err += Math.sqrt(bestD);
  }
  if (err / levels.length > 24) return; // not jet: leave it alone
  const out = new Uint32Array(d.buffer);
  for (let p = 0; p < levels.length; p++) out[p] = WATERFALL_LUT[levels[p]];
  ctx.putImageData(img, 0, 0);
}

/**
 * The recorded spectrogram arrives in the receiver's own orientation (one
 * row per minute, oldest at the top, frequency across). Turn it so time runs
 * left to right and frequency upwards, matching how the strip is labelled.
 */
export async function loadHistory(url: string, canvas: HTMLCanvasElement): Promise<boolean> {
  try {
    const res = await fetch(url, { cache: "no-cache" });
    if (!res.ok) return false;
    const bmp = await createImageBitmap(await res.blob());
    canvas.width = bmp.height;
    canvas.height = bmp.width;
    const ctx = canvas.getContext("2d", { willReadFrequently: true });
    if (!ctx) return false;
    ctx.imageSmoothingEnabled = false;
    ctx.setTransform(0, -1, 1, 0, 0, bmp.width);
    ctx.drawImage(bmp, 0, 0);
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    bmp.close();
    if (canvas.width * canvas.height <= 200_000) recolourJet(ctx, canvas.width, canvas.height);
    return true;
  } catch {
    return false;
  }
}
