// The schedule tables. Each transmission is its own <tbody>, one row per
// frequency, so multi-frequency entries line up and the alternating bands
// follow transmissions rather than lines.

import type { EventFreq, ScheduleEvent } from "./types";
import { h } from "./dom";
import { kHz, localDayTag, localHM, localZone, now, span, ts, utcDayTag, utcHM } from "./format";

export type Section = "now" | "next" | "later";

export interface RowActions {
  play(ev: ScheduleEvent, f: EventFreq): void;
  remind(ev: ScheduleEvent): void;
  playing(): { eventId: string | null; freqHz: number; state: string };
  reminderSet(id: string): boolean;
  remindersSupported(): boolean;
}

/** Play is offered for live transmissions and those starting within this. */
const PLAY_LEAD_MS = 15 * 60_000;

function head(): HTMLTableSectionElement {
  return h(
    "thead",
    null,
    h(
      "tr",
      null,
      h("th", { scope: "col", class: "c-utc" }, "UTC"),
      h("th", { scope: "col", class: "c-loc" }, localZone()),
      h("th", { scope: "col", class: "c-count" }, h("span", { class: "vh" }, "Countdown")),
      h("th", { scope: "col", class: "c-station" }, "Station"),
      h("th", { scope: "col", class: "c-freq" }, "kHz"),
      h("th", { scope: "col", class: "c-mode" }, "Mode"),
      h("th", { scope: "col", class: "c-tgt" }, "Target and remarks"),
      h("th", { scope: "col", class: "c-sig" }, "Signal"),
      h("th", { scope: "col", class: "c-rx" }, "Best receiver"),
      h("th", { scope: "col", class: "c-act" }, h("span", { class: "vh" }, "Actions")),
    ),
  );
}

export function countdownText(section: Section, startMs: number, endMs: number, t: number): string {
  if (section === "now") return t < endMs ? `${span(endMs - t)} left` : "ending";
  return t < startMs ? `in ${span(startMs - t)}` : "due now";
}

function timeCells(ev: ScheduleEvent, section: Section, rows: number): HTMLTableCellElement[] {
  const s = ts(ev.start);
  const e = ts(ev.end);
  const ud = utcDayTag(s);
  const ld = localDayTag(s);
  return [
    h("td", { class: "c-utc", rowspan: rows }, ud ? h("span", { class: "day" }, ud) : null, h("time", { datetime: ev.start, class: "mono" }, utcHM(s))),
    h("td", { class: "c-loc", rowspan: rows }, ld ? h("span", { class: "day" }, ld) : null, h("span", { class: "mono" }, localHM(s))),
    h("td", { class: "c-count", rowspan: rows }, h("span", { class: "mono count", "data-section": section, "data-start": s, "data-end": e }, countdownText(section, s, e, now()))),
  ];
}

function stationCell(ev: ScheduleEvent, rows: number): HTMLTableCellElement {
  return h(
    "th",
    { scope: "rowgroup", class: "c-station", rowspan: rows },
    h("span", { class: "des mono" }, ev.station),
    ev.stationName ? h("span", { class: "sname" }, ev.stationName) : null,
    ev.priyomUrl ? h("a", { class: "pri", href: ev.priyomUrl, rel: "noopener", target: "_blank" }, "Priyom") : null,
  );
}

function signalCell(f: EventFreq | null): HTMLTableCellElement {
  if (!f) return h("td", { class: "c-sig" }, h("span", { class: "sig sig-none" }, "No frequency"));
  const s = f.signal;
  const at = s.at ? ` at ${utcHM(ts(s.at))} UTC` : "";
  const rx = s.receiverKey ? f.receivers.find((r) => r.key === s.receiverKey)?.callsign : undefined;
  const where = rx ? ` on ${rx}` : "";
  if (s.state === "present") {
    return h(
      "td",
      { class: "c-sig" },
      h("span", { class: "sig sig-present", title: `Carrier detected${at}${where}` }, h("span", { class: "sig-mark", "aria-hidden": "true" }), "Heard", typeof s.snr === "number" ? h("span", { class: "mono snr" }, `${Math.round(s.snr)} dB`) : null),
    );
  }
  if (s.state === "absent") {
    return h("td", { class: "c-sig" }, h("span", { class: "sig sig-absent", title: `Nothing detected${at}${where}` }, h("span", { class: "sig-mark", "aria-hidden": "true" }), "Not heard"));
  }
  return h("td", { class: "c-sig" }, h("span", { class: "sig sig-unknown", title: "No signal check yet" }, h("span", { class: "sig-mark", "aria-hidden": "true" }), "Not checked"));
}

function receiverCell(f: EventFreq | null): HTMLTableCellElement {
  const r = f?.receivers[0];
  if (!f) return h("td", { class: "c-rx" });
  if (!r) return h("td", { class: "c-rx" }, h("span", { class: "muted" }, "No receiver covers this"));
  return h(
    "td",
    { class: "c-rx" },
    h("span", { class: "rx-c mono" }, r.callsign),
    h("span", { class: "rx-l", title: r.location }, r.location),
  );
}

function remarksCell(ev: ScheduleEvent, rows: number): HTMLTableCellElement {
  return h(
    "td",
    { class: "c-tgt", rowspan: rows },
    ev.target ? h("span", { class: "tgt" }, ev.target) : null,
    ...ev.remarks.map((r) => h("span", { class: "rem" }, r)),
  );
}

function modeCell(ev: ScheduleEvent, rows: number): HTMLTableCellElement {
  return h("td", { class: "c-mode", rowspan: rows }, ev.priyomMode ? h("span", { class: "mono" }, ev.priyomMode) : null, ev.digital ? h("span", { class: "digital" }, "data mode") : null);
}

function remindButton(ev: ScheduleEvent, section: Section, a: RowActions): HTMLElement | null {
  if (section === "now" || !a.remindersSupported()) return null;
  const set = a.reminderSet(ev.id);
  return h(
    "button",
    {
      type: "button",
      class: `btn btn-small btn-quiet remind${set ? " is-set" : ""}`,
      "aria-pressed": set ? "true" : "false",
      "aria-label": `${set ? "Cancel reminder for" : "Remind me about"} ${ev.station} at ${utcHM(ts(ev.start))} UTC`,
      "data-fk": `remind:${ev.id}`,
      onclick: () => a.remind(ev),
    },
    set ? "Reminder set" : "Remind me",
  );
}

function playButton(ev: ScheduleEvent, f: EventFreq, section: Section, a: RowActions): HTMLElement | null {
  const t = now();
  const startsSoon = ts(ev.start) - t <= PLAY_LEAD_MS;
  if (section !== "now" && !startsSoon) return null;
  const p = a.playing();
  const isCur = p.eventId === ev.id && p.freqHz === f.hz;
  const active = isCur && (p.state === "playing" || p.state === "connecting");
  const label = active ? "Pause" : isCur ? "Resume" : "Play";
  return h(
    "button",
    {
      type: "button",
      class: `btn btn-small ${active ? "btn-on" : "btn-primary"} play`,
      "aria-label": `${label} ${ev.station} on ${kHz(f.hz)} kHz`,
      "data-fk": `play:${ev.id}:${f.hz}`,
      disabled: f.receivers.length === 0,
      onclick: () => a.play(ev, f),
    },
    label,
  );
}

function eventBody(ev: ScheduleEvent, section: Section, a: RowActions): HTMLTableSectionElement {
  const p = a.playing();
  const body = h("tbody", { "data-event": ev.id, class: `ev${p.eventId === ev.id && p.state !== "idle" ? " is-cur" : ""}` });

  if (!ev.parsed) {
    body.classList.add("ev-raw");
    body.append(
      h(
        "tr",
        null,
        ...timeCells(ev, section, 1),
        stationCell(ev, 1),
        h("td", { class: "c-rawtext", colspan: 5 }, h("span", { class: "raw mono" }, ev.raw), h("span", { class: "rem" }, "Shown as written in the schedule; it could not be read as a frequency and mode.")),
        h("td", { class: "c-act" }, remindButton(ev, section, a)),
      ),
    );
    return body;
  }

  if (ev.search || ev.freqs.length === 0) {
    body.classList.add("ev-search");
    body.append(
      h(
        "tr",
        null,
        ...timeCells(ev, section, 1),
        stationCell(ev, 1),
        h("td", { class: "c-freq" }, h("span", { class: "search" }, "Search")),
        modeCell(ev, 1),
        remarksCell(ev, 1),
        h("td", { class: "c-sig" }, h("span", { class: "muted" }, "No frequency listed")),
        h("td", { class: "c-rx" }),
        h("td", { class: "c-act" }, remindButton(ev, section, a)),
      ),
    );
    return body;
  }

  const n = ev.freqs.length;
  ev.freqs.forEach((f, i) => {
    const isCur = p.eventId === ev.id && p.freqHz === f.hz && p.state !== "idle";
    const tr = h("tr", { class: isCur ? "is-playing" : null, "data-freq": f.hz });
    if (i === 0) tr.append(...timeCells(ev, section, n), stationCell(ev, n));
    tr.append(h("td", { class: "c-freq" }, h("span", { class: "mono freq" }, kHz(f.hz))));
    if (i === 0) tr.append(modeCell(ev, n), remarksCell(ev, n));
    tr.append(signalCell(f), receiverCell(f), h("td", { class: "c-act" }, playButton(ev, f, section, a), i === 0 ? remindButton(ev, section, a) : null));
    body.append(tr);
  });
  return body;
}

function emptyBody(section: Section, next: ScheduleEvent | undefined): HTMLTableSectionElement {
  let text = "Nothing more is scheduled in the next 24 hours.";
  if (section === "now") {
    text = next ? `Nothing scheduled is on the air right now. The next transmission, ${next.station}, starts at ${utcHM(ts(next.start))} UTC.` : "Nothing scheduled is on the air right now.";
  } else if (section === "next") {
    text = "No more transmissions are scheduled today.";
  }
  return h("tbody", { class: "ev-empty" }, h("tr", null, h("td", { colspan: 10 }, text)));
}

export function renderSection(table: HTMLTableElement, count: HTMLElement, section: Section, events: ScheduleEvent[], a: RowActions, nextUp?: ScheduleEvent): void {
  const bodies = events.length ? events.map((e) => eventBody(e, section, a)) : [emptyBody(section, nextUp)];
  table.replaceChildren(head(), ...bodies);
  count.textContent = events.length ? String(events.length) : "";
}

export function tickCountdowns(root: ParentNode): void {
  const t = now();
  root.querySelectorAll<HTMLElement>(".count[data-start]").forEach((el) => {
    const s = Number(el.dataset.start);
    const e = Number(el.dataset.end);
    const txt = countdownText(el.dataset.section as Section, s, e, t);
    if (el.textContent !== txt) el.textContent = txt;
  });
}
