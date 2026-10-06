// The schedule. Entries with the same station and start time are grouped
// into one transmission with several frequencies (Priyom lists each
// frequency as its own entry).
//
// Wide screens get a table: each transmission is its own <tbody>, one row
// per frequency, so the alternating bands follow transmissions rather than
// lines. Narrow screens get a compact list: two lines per transmission
// (time, station and countdown; then one chip per frequency), with the
// details behind a tap.

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

/** One frequency of a grouped transmission, with the feed entry it came from. */
interface Item {
  ev: ScheduleEvent;
  f: EventFreq;
}

/** Feed entries sharing a station and start time. */
export interface Group {
  key: string;
  lead: ScheduleEvent;
  events: ScheduleEvent[];
  items: Item[];
  startMs: number;
  endMs: number;
  modes: string[];
  digital: boolean;
  target: string | null;
  remarks: string[];
}

export function groupEvents(events: ScheduleEvent[]): Group[] {
  const out: Group[] = [];
  const byKey = new Map<string, Group>();
  for (const ev of events) {
    const groupable = ev.parsed && !ev.search && ev.freqs.length > 0;
    const key = groupable ? `${ev.station}|${ev.start}` : `single|${ev.id}`;
    let g = byKey.get(key);
    if (!g) {
      g = { key, lead: ev, events: [], items: [], startMs: ts(ev.start), endMs: ts(ev.end), modes: [], digital: false, target: null, remarks: [] };
      byKey.set(key, g);
      out.push(g);
    }
    g.events.push(ev);
    g.endMs = Math.max(g.endMs, ts(ev.end));
    for (const f of ev.freqs) g.items.push({ ev, f });
    if (ev.priyomMode && !g.modes.includes(ev.priyomMode)) g.modes.push(ev.priyomMode);
    g.digital ||= ev.digital;
    g.target ??= ev.target;
    for (const r of otherRemarks(ev)) if (!g.remarks.includes(r)) g.remarks.push(r);
  }
  return out;
}

/** Remarks without the "Target: ..." one, which is shown on its own. */
function otherRemarks(ev: ScheduleEvent): string[] {
  return ev.remarks.filter((r) => !(ev.target && /^target\s*:/i.test(r)));
}

/** The group as one event, for reminders: every frequency, the lead's id. */
export function mergedEvent(g: Group): ScheduleEvent {
  return { ...g.lead, freqs: g.items.map((i) => i.f), end: new Date(g.endMs).toISOString() };
}

export function countdownText(section: Section, startMs: number, endMs: number, t: number): string {
  if (section === "now") return t < endMs ? `${span(endMs - t)} left` : "ending";
  return t < startMs ? `in ${span(startMs - t)}` : "due now";
}

function countSpan(section: Section, g: Group): HTMLElement {
  return h("span", { class: "mono count", "data-section": section, "data-start": g.startMs, "data-end": g.endMs }, countdownText(section, g.startMs, g.endMs, now()));
}

function canPlay(g: Group, section: Section): boolean {
  return section === "now" || g.startMs - now() <= PLAY_LEAD_MS;
}

function playState(a: RowActions, it: Item): { cur: boolean; active: boolean; label: string } {
  const p = a.playing();
  const cur = p.eventId === it.ev.id && p.freqHz === it.f.hz && p.state !== "idle";
  const active = cur && (p.state === "playing" || p.state === "connecting");
  return { cur, active, label: active ? "Pause" : cur ? "Resume" : "Play" };
}

function isCurrent(g: Group, a: RowActions): boolean {
  const p = a.playing();
  return p.state !== "idle" && g.events.some((e) => e.id === p.eventId);
}

function signalText(f: EventFreq): { cls: string; text: string; title: string } {
  const s = f.signal;
  const at = s.at ? ` at ${utcHM(ts(s.at))} UTC` : "";
  const rx = s.receiverKey ? f.receivers.find((r) => r.key === s.receiverKey)?.callsign : undefined;
  const where = rx ? ` on ${rx}` : "";
  if (s.state === "present") return { cls: "sig-present", text: typeof s.snr === "number" ? `Heard ${Math.round(s.snr)} dB` : "Heard", title: `Carrier detected${at}${where}` };
  if (s.state === "absent") return { cls: "sig-absent", text: "Not heard", title: `Nothing detected${at}${where}` };
  return { cls: "sig-unknown", text: "Not checked", title: "No signal check yet" };
}

function remindButton(g: Group, section: Section, a: RowActions, extraClass = ""): HTMLElement | null {
  if (section === "now" || !a.remindersSupported()) return null;
  const ev = g.lead;
  const set = a.reminderSet(ev.id);
  return h(
    "button",
    {
      type: "button",
      class: `btn btn-small btn-quiet remind${set ? " is-set" : ""}${extraClass}`,
      "aria-pressed": set ? "true" : "false",
      "aria-label": `${set ? "Cancel reminder for" : "Remind me about"} ${ev.station} at ${utcHM(g.startMs)} UTC`,
      "data-fk": `remind:${ev.id}`,
      onclick: () => a.remind(mergedEvent(g)),
    },
    set ? "Reminder set" : "Remind me",
  );
}

// ---------------------------------------------------------------- table

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

function timeCells(g: Group, section: Section, rows: number): HTMLTableCellElement[] {
  const ud = utcDayTag(g.startMs);
  const ld = localDayTag(g.startMs);
  return [
    h("td", { class: "c-utc", rowspan: rows }, ud ? h("span", { class: "day" }, ud) : null, h("time", { datetime: g.lead.start, class: "mono" }, utcHM(g.startMs))),
    h("td", { class: "c-loc", rowspan: rows }, ld ? h("span", { class: "day" }, ld) : null, h("span", { class: "mono" }, localHM(g.startMs))),
    h("td", { class: "c-count", rowspan: rows }, countSpan(section, g)),
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

function signalCell(f: EventFreq): HTMLTableCellElement {
  const s = signalText(f);
  return h("td", { class: "c-sig" }, h("span", { class: `sig ${s.cls}`, title: s.title }, h("span", { class: "sig-mark", "aria-hidden": "true" }), s.text));
}

function receiverCell(f: EventFreq): HTMLTableCellElement {
  const r = f.receivers[0];
  if (!r) return h("td", { class: "c-rx" }, h("span", { class: "muted" }, "No receiver covers this"));
  return h("td", { class: "c-rx" }, h("span", { class: "rx-c mono" }, r.callsign), h("span", { class: "rx-l", title: r.location }, r.location));
}

function remarksCell(g: Group, rows: number): HTMLTableCellElement {
  return h("td", { class: "c-tgt", rowspan: rows }, g.target ? h("span", { class: "tgt" }, g.target) : null, ...g.remarks.map((r) => h("span", { class: "rem" }, r)));
}

function modeCell(g: Group, rows: number): HTMLTableCellElement {
  return h("td", { class: "c-mode", rowspan: rows }, g.modes.length ? h("span", { class: "mono" }, g.modes.join(", ")) : null, g.digital ? h("span", { class: "digital" }, "data mode") : null);
}

function playButton(it: Item, g: Group, section: Section, a: RowActions): HTMLElement | null {
  if (!canPlay(g, section)) return null;
  const st = playState(a, it);
  return h(
    "button",
    {
      type: "button",
      class: `btn btn-small ${st.active ? "btn-on" : "btn-primary"} play`,
      "aria-label": `${st.label} ${it.ev.station} on ${kHz(it.f.hz)} kHz`,
      "data-fk": `play:${it.ev.id}:${it.f.hz}`,
      disabled: it.f.receivers.length === 0,
      onclick: () => a.play(it.ev, it.f),
    },
    st.label,
  );
}

function groupBody(g: Group, section: Section, a: RowActions): HTMLTableSectionElement {
  const ev = g.lead;
  const body = h("tbody", { "data-event": ev.id, class: `ev${isCurrent(g, a) ? " is-cur" : ""}` });

  if (!ev.parsed) {
    body.classList.add("ev-raw");
    body.append(
      h(
        "tr",
        null,
        ...timeCells(g, section, 1),
        stationCell(ev, 1),
        h("td", { class: "c-rawtext", colspan: 5 }, h("span", { class: "raw mono" }, ev.raw), h("span", { class: "rem" }, "Shown as written in the schedule; it could not be read as a frequency and mode.")),
        h("td", { class: "c-act" }, remindButton(g, section, a)),
      ),
    );
    return body;
  }

  if (g.items.length === 0) {
    body.classList.add("ev-search");
    body.append(
      h(
        "tr",
        null,
        ...timeCells(g, section, 1),
        stationCell(ev, 1),
        h("td", { class: "c-freq" }, h("span", { class: "search" }, "Search")),
        modeCell(g, 1),
        remarksCell(g, 1),
        h("td", { class: "c-sig" }, h("span", { class: "muted" }, "No frequency listed")),
        h("td", { class: "c-rx" }),
        h("td", { class: "c-act" }, remindButton(g, section, a)),
      ),
    );
    return body;
  }

  const n = g.items.length;
  g.items.forEach((it, i) => {
    const tr = h("tr", { class: playState(a, it).cur ? "is-playing" : null, "data-freq": it.f.hz });
    if (i === 0) tr.append(...timeCells(g, section, n), stationCell(ev, n));
    tr.append(h("td", { class: "c-freq" }, h("span", { class: "mono freq" }, kHz(it.f.hz))));
    if (i === 0) tr.append(modeCell(g, n), remarksCell(g, n));
    tr.append(signalCell(it.f), receiverCell(it.f), h("td", { class: "c-act" }, playButton(it, g, section, a), i === 0 ? remindButton(g, section, a) : null));
    body.append(tr);
  });
  return body;
}

function emptyText(section: Section, next: ScheduleEvent | undefined): string {
  if (section === "now") {
    return next ? `Nothing scheduled is on the air right now. The next transmission, ${next.station}, starts at ${utcHM(ts(next.start))} UTC.` : "Nothing scheduled is on the air right now.";
  }
  if (section === "next") return "No more transmissions are scheduled today.";
  return "Nothing more is scheduled in the next 24 hours.";
}

export function renderTable(table: HTMLTableElement, section: Section, groups: Group[], a: RowActions, nextUp?: ScheduleEvent): void {
  const bodies = groups.length
    ? groups.map((g) => groupBody(g, section, a))
    : [h("tbody", { class: "ev-empty" }, h("tr", null, h("td", { colspan: 10 }, emptyText(section, nextUp))))];
  table.replaceChildren(head(), ...bodies);
}

// ---------------------------------------------------------------- compact list

/** Which transmissions are expanded in the compact list; survives re-renders. */
const expanded = new Set<string>();

function chip(it: Item, g: Group, section: Section, a: RowActions): HTMLElement {
  const sig = signalText(it.f);
  const showSig = section === "now";
  const mark = showSig ? h("span", { class: `chip-sig ${sig.cls}`, "aria-hidden": "true" }) : null;
  const label = h("span", { class: "mono" }, kHz(it.f.hz));
  if (!canPlay(g, section) || it.f.receivers.length === 0) {
    return h("span", { class: "chip chip-static" }, mark, label);
  }
  const st = playState(a, it);
  return h(
    "button",
    {
      type: "button",
      class: `chip${st.active ? " chip-on" : ""}${st.cur && !st.active ? " chip-cur" : ""}`,
      "aria-label": `${st.label} ${it.ev.station} on ${kHz(it.f.hz)} kHz${showSig ? `, ${sig.text}` : ""}`,
      "data-fk": `play:${it.ev.id}:${it.f.hz}`,
      onclick: () => a.play(it.ev, it.f),
    },
    h("span", { class: `chip-icon ${st.active ? "i-pause" : "i-play"}`, "aria-hidden": "true" }),
    label,
    mark,
  );
}

function detailLines(g: Group, section: Section, a: RowActions): HTMLElement[] {
  const ev = g.lead;
  const lines: HTMLElement[] = [];
  const ld = localDayTag(g.startMs);
  lines.push(
    h(
      "p",
      { class: "tx-d" },
      `Starts ${utcHM(g.startMs)} UTC, ${ld ? `${ld} ` : ""}${localHM(g.startMs)} ${localZone()}. ${section === "now" ? "Ends" : "Expected to end"} around ${utcHM(g.endMs)} UTC${g.lead.endEstimated ? " (estimated)" : ""}.`,
    ),
  );
  if (!ev.parsed) {
    lines.push(h("p", { class: "tx-d mono tx-raw" }, ev.raw), h("p", { class: "tx-d muted" }, "Shown as written in the schedule; it could not be read as a frequency and mode."));
  }
  if (g.target) lines.push(h("p", { class: "tx-d" }, `Target: ${g.target}`));
  for (const r of g.remarks) lines.push(h("p", { class: "tx-d muted" }, r));
  if (g.digital) lines.push(h("p", { class: "tx-d muted" }, "A data mode: you hear the raw tones; decoding needs separate software."));
  for (const it of g.items) {
    const sig = signalText(it.f);
    const r = it.f.receivers[0];
    lines.push(
      h(
        "p",
        { class: "tx-d tx-freq" },
        h("span", { class: "mono" }, `${kHz(it.f.hz)} kHz`),
        h("span", { class: `sig ${sig.cls}` }, h("span", { class: "sig-mark", "aria-hidden": "true" }), sig.text),
        r ? h("span", null, h("span", { class: "mono" }, r.callsign), ` ${r.location}`) : h("span", { class: "muted" }, "No receiver covers this"),
      ),
    );
  }
  const links = h("p", { class: "tx-d tx-actions" });
  if (ev.priyomUrl) links.append(h("a", { href: ev.priyomUrl, rel: "noopener", target: "_blank" }, "Station page on Priyom.org"));
  const rb = remindButton(g, section, a);
  if (rb) links.append(rb);
  if (links.childNodes.length) lines.push(links);
  return lines;
}

function listItem(g: Group, section: Section, a: RowActions): HTMLLIElement {
  const ev = g.lead;
  const open = expanded.has(g.key);
  const id = `tx-${section}-${ev.id}`;
  const ud = utcDayTag(g.startMs);
  const li = h("li", { class: `tx${isCurrent(g, a) ? " is-cur" : ""}${open ? " is-open" : ""}`, "data-event": ev.id });

  const more = h("div", { class: "tx-more", id }, ...(open ? detailLines(g, section, a) : []));
  more.hidden = !open;
  const toggle = h(
    "button",
    {
      type: "button",
      class: "tx-head",
      "aria-expanded": open ? "true" : "false",
      "aria-controls": id,
      "data-fk": `tx:${g.key}`,
      onclick: () => {
        const now = !expanded.has(g.key);
        if (now) expanded.add(g.key);
        else expanded.delete(g.key);
        li.classList.toggle("is-open", now);
        toggle.setAttribute("aria-expanded", now ? "true" : "false");
        more.replaceChildren(...(now ? detailLines(g, section, a) : []));
        more.hidden = !now;
      },
    },
    h("time", { class: "tx-time mono", datetime: ev.start }, ud ? h("span", { class: "day" }, `${ud} `) : null, utcHM(g.startMs)),
    h("span", { class: "tx-des mono" }, ev.station),
    h("span", { class: "tx-name" }, ev.stationName || ""),
    countSpan(section, g),
  );

  let line2: HTMLElement;
  if (!ev.parsed) {
    line2 = h("div", { class: "tx-freqs" }, h("span", { class: "tx-raw mono" }, ev.raw));
  } else if (g.items.length === 0) {
    line2 = h("div", { class: "tx-freqs" }, h("span", { class: "search" }, "Search"), g.remarks[0] ? h("span", { class: "tx-mode muted" }, g.remarks[0]) : null);
  } else {
    line2 = h(
      "div",
      { class: "tx-freqs" },
      ...g.items.map((it) => chip(it, g, section, a)),
      h("span", { class: "tx-mode mono" }, g.modes.join(", ") + (g.digital ? " data" : "")),
    );
  }
  li.append(toggle, line2, more);
  return li;
}

export function renderList(list: HTMLOListElement, section: Section, groups: Group[], a: RowActions, nextUp?: ScheduleEvent): void {
  if (groups.length === 0) {
    list.replaceChildren(h("li", { class: "tx-empty" }, emptyText(section, nextUp)));
    return;
  }
  list.replaceChildren(...groups.map((g) => listItem(g, section, a)));
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
