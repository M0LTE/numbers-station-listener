// Waterfall colour map. A sequential map that rises steadily in lightness
// from near black through blue and teal to pale mint (close to seaborn's
// "mako"), so a stronger signal always reads as lighter, also in greyscale
// and for colour-blind viewers. The tuned-frequency marker is drawn in the
// page's red accent, which sits outside this map's hues.

const STOPS: Array<[number, number, number, number]> = [
  [0.0, 0x0b, 0x04, 0x05],
  [0.14, 0x2a, 0x1b, 0x3a],
  [0.3, 0x3b, 0x3d, 0x80],
  [0.46, 0x37, 0x6a, 0x9f],
  [0.62, 0x34, 0x98, 0xa9],
  [0.78, 0x5c, 0xc6, 0xac],
  [0.9, 0xa8, 0xe1, 0xbc],
  [1.0, 0xde, 0xf5, 0xe5],
];

function buildRGB(): Uint8Array {
  const rgb = new Uint8Array(256 * 3);
  for (let i = 0; i < 256; i++) {
    const t = i / 255;
    let k = 0;
    while (k < STOPS.length - 2 && t > STOPS[k + 1][0]) k++;
    const [t0, r0, g0, b0] = STOPS[k];
    const [t1, r1, g1, b1] = STOPS[k + 1];
    const f = (t - t0) / (t1 - t0);
    rgb[i * 3] = Math.round(r0 + (r1 - r0) * f);
    rgb[i * 3 + 1] = Math.round(g0 + (g1 - g0) * f);
    rgb[i * 3 + 2] = Math.round(b0 + (b1 - b0) * f);
  }
  return rgb;
}

/** 256 RGB triples, index = level 0..255. */
export const WATERFALL_RGB = buildRGB();

function pack(): Uint32Array {
  const lut = new Uint32Array(256);
  const littleEndian = new Uint8Array(new Uint32Array([1]).buffer)[0] === 1;
  for (let i = 0; i < 256; i++) {
    const r = WATERFALL_RGB[i * 3];
    const g = WATERFALL_RGB[i * 3 + 1];
    const b = WATERFALL_RGB[i * 3 + 2];
    lut[i] = littleEndian ? ((255 << 24) | (b << 16) | (g << 8) | r) >>> 0 : ((r << 24) | (g << 16) | (b << 8) | 255) >>> 0;
  }
  return lut;
}

/** 256 packed RGBA pixels, ready to write into an ImageData's Uint32 view. */
export const WATERFALL_LUT = pack();
