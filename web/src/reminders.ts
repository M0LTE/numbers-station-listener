// "Remind me" for upcoming transmissions. Scheduled in the page with
// setTimeout, so the tab has to stay open; kept in localStorage so a reload
// keeps them. Permission is asked for only when someone clicks the button.

import { kHz, now, ts, utcHM } from "./format";
import type { ScheduleEvent } from "./types";

interface Reminder {
  id: string;
  startMs: number;
  title: string;
  body: string;
}

const KEY = "nsl.reminders";
const LEAD_MS = 60_000;

const timers = new Map<string, number>();
let reminders = new Map<string, Reminder>();

function load(): void {
  try {
    const raw = localStorage.getItem(KEY);
    const list = raw ? (JSON.parse(raw) as Reminder[]) : [];
    reminders = new Map(list.filter((r) => r && typeof r.id === "string" && r.startMs > now()).map((r) => [r.id, r]));
  } catch {
    reminders = new Map();
  }
}

function save(): void {
  try {
    localStorage.setItem(KEY, JSON.stringify([...reminders.values()]));
  } catch {
    /* private window or storage blocked: reminders last until the tab closes */
  }
}

function arm(r: Reminder): void {
  window.clearTimeout(timers.get(r.id));
  const delay = Math.max(0, r.startMs - LEAD_MS - now());
  timers.set(
    r.id,
    window.setTimeout(() => {
      timers.delete(r.id);
      reminders.delete(r.id);
      save();
      fire(r);
      onChange();
    }, Math.min(delay, 2 ** 31 - 1)),
  );
}

function fire(r: Reminder): void {
  try {
    if (Notification.permission !== "granted") return;
    const n = new Notification(r.title, { body: r.body, tag: `nsl-${r.id}` });
    n.onclick = () => {
      window.focus();
      document.querySelector(`[data-event="${CSS.escape(r.id)}"]`)?.scrollIntoView({ block: "center" });
      n.close();
    };
  } catch {
    /* notifications unavailable */
  }
}

let onChange: () => void = () => {};

export function initReminders(changed: () => void): void {
  onChange = changed;
  load();
  for (const r of reminders.values()) arm(r);
  save();
}

export function supported(): boolean {
  return typeof window.Notification === "function";
}

export function isSet(id: string): boolean {
  return reminders.has(id);
}

/** Returns a message to show if the reminder could not be set. */
export async function toggle(ev: ScheduleEvent): Promise<string | null> {
  if (reminders.has(ev.id)) {
    window.clearTimeout(timers.get(ev.id));
    timers.delete(ev.id);
    reminders.delete(ev.id);
    save();
    onChange();
    return null;
  }
  if (!supported()) return "This browser cannot show notifications.";
  let perm = Notification.permission;
  if (perm === "default") {
    try {
      perm = await Notification.requestPermission();
    } catch {
      perm = "denied";
    }
  }
  if (perm !== "granted") return "Notifications are blocked for this site. Allow them in your browser's site settings to use reminders.";
  const freqs = ev.freqs.map((f) => kHz(f.hz)).join(", ");
  const r: Reminder = {
    id: ev.id,
    startMs: ts(ev.start),
    title: `${ev.station} starts at ${utcHM(ts(ev.start))} UTC`,
    body: [ev.stationName, freqs ? `${freqs} kHz ${ev.priyomMode}` : "Frequency not listed", ev.target ? `Target: ${ev.target}` : ""].filter(Boolean).join("\n"),
  };
  reminders.set(r.id, r);
  save();
  arm(r);
  onChange();
  return null;
}
