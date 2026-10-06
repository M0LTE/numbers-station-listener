// Time and frequency formatting. Countdowns use the server's clock, corrected
// by the offset seen on the last /api/now payload, so a skewed PC clock does
// not make a transmission look early or late.

let offsetMs = 0;

export function setServerTime(iso: string): void {
  const t = ts(iso);
  if (!Number.isNaN(t)) offsetMs = t - Date.now();
}

/**
 * Parses an RFC 3339 time. The server may send nanosecond fractions, which
 * not every browser's Date.parse accepts, so cut them to milliseconds.
 */
export function ts(iso: string): number {
  return Date.parse(iso.replace(/(\.\d{3})\d+/, "$1"));
}

export function now(): number {
  return Date.now() + offsetMs;
}

const pad = (n: number): string => String(n).padStart(2, "0");

export function utcHM(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}

export function utcHMS(ms: number): string {
  const d = new Date(ms);
  return `${utcHM(ms)}:${pad(d.getUTCSeconds())}`;
}

export function localHM(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function localHMS(ms: number): string {
  const d = new Date(ms);
  return `${localHM(ms)}:${pad(d.getSeconds())}`;
}

/** Short weekday when the UTC date differs from today's UTC date. */
export function utcDayTag(ms: number): string {
  const d = new Date(ms);
  const t = new Date(now());
  if (d.getUTCFullYear() === t.getUTCFullYear() && d.getUTCMonth() === t.getUTCMonth() && d.getUTCDate() === t.getUTCDate()) {
    return "";
  }
  return d.toLocaleDateString("en-GB", { weekday: "short", timeZone: "UTC" });
}

export function localDayTag(ms: number): string {
  const d = new Date(ms);
  const t = new Date(now());
  if (d.getFullYear() === t.getFullYear() && d.getMonth() === t.getMonth() && d.getDate() === t.getDate()) {
    return "";
  }
  return d.toLocaleDateString(undefined, { weekday: "short" });
}

let tzName: string | null = null;
/** The browser's short zone name, e.g. "BST", or "local" if it has none. */
export function localZone(): string {
  if (tzName !== null) return tzName;
  // Prefer a real abbreviation ("BST", "EDT") over "GMT+1"; locales differ
  // in which zones they have names for.
  const names: string[] = [];
  for (const loc of [undefined, "en-GB", "en-US"]) {
    try {
      const part = new Intl.DateTimeFormat(loc, { timeZoneName: "short" })
        .formatToParts(new Date())
        .find((p) => p.type === "timeZoneName");
      if (part) names.push(part.value);
    } catch {
      /* ignore this locale */
    }
  }
  tzName = names.find((n) => !/^(GMT|UTC)[+-]/.test(n)) ?? names[0] ?? "Local";
  return tzName;
}

/** "12:04" under an hour, "2 h 13 min" beyond. */
export function span(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 3600) return `${pad(Math.floor(s / 60))}:${pad(s % 60)}`;
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return `${h} h ${pad(m)} min`;
}

export function kHz(hz: number): string {
  const k = hz / 1000;
  return Number.isInteger(k) ? String(k) : k.toFixed(1);
}

export function modeLabel(mode: string): string {
  return mode.toUpperCase();
}
