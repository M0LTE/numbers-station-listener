import "@fontsource-variable/archivo/wdth.css";
import "@fontsource-variable/martian-mono/wdth.css";
import "./style.css";

import type { NowResponse, ScheduleEvent } from "./types";
import { $ } from "./dom";
import { localHMS, localZone, now, setServerTime, ts, utcHM, utcHMS } from "./format";
import { startFeed, type FeedMode } from "./live";
import { Player } from "./player";
import { initReminders, isSet, supported as remindersSupported, toggle as toggleReminder } from "./reminders";
import { groupEvents, renderList, renderTable, tickCountdowns, type RowActions, type Section } from "./schedule";
import { isNarrow, narrowMq } from "./layout";

let data: NowResponse | null = null;
let feedMode: FeedMode = "connecting";

// On a phone "Later today" starts folded away; the choice is remembered.
const LATER_KEY = "nsl.laterOpen";
let laterOpen = false;
try {
  laterOpen = localStorage.getItem(LATER_KEY) === "1";
} catch {
  /* storage blocked: stay folded */
}

const player = new Player();

const actions: RowActions = {
  play(ev, f) {
    const p = player.current;
    if (p.eventId === ev.id && p.freqHz === f.hz && (p.state === "playing" || p.state === "connecting")) {
      player.pause();
    } else {
      player.open({ kind: "event", ev, freqHz: f.hz });
    }
  },
  remind(ev) {
    void toggleReminder(ev).then((msg) => notice(msg));
  },
  playing: () => player.current,
  reminderSet: isSet,
  remindersSupported,
};

function notice(msg: string | null): void {
  const n = $("notice");
  n.textContent = msg ?? "";
  n.hidden = !msg;
}

function render(): void {
  if (!data) return;
  // Re-rendering replaces the buttons; keep keyboard focus where it was.
  const fk = (document.activeElement as HTMLElement | null)?.dataset?.fk;
  const all: ScheduleEvent[] = [...data.now, ...data.next, ...data.later];
  const narrow = isNarrow();
  const sections: Array<[Section, ScheduleEvent[]]> = [
    ["now", data.now],
    ["next", data.next],
    ["later", data.later],
  ];
  for (const [sec, events] of sections) {
    const groups = groupEvents(events);
    const table = $(`t-${sec}`) as HTMLTableElement;
    const list = $(`l-${sec}`) as HTMLOListElement;
    $(`c-${sec}`).textContent = groups.length ? String(groups.length) : "";
    const folded = narrow && sec === "later" && !laterOpen && groups.length > 0;
    if (sec === "later") {
      const btn = $("later-toggle");
      btn.hidden = !narrow || groups.length === 0;
      btn.textContent = folded ? `Show ${groups.length} more` : "Show fewer";
      btn.setAttribute("aria-expanded", folded ? "false" : "true");
    }
    if (narrow) {
      table.replaceChildren();
      table.hidden = true;
      list.hidden = folded;
      if (folded) list.replaceChildren();
      else renderList(list, sec, groups, actions, data.next[0]);
    } else {
      list.replaceChildren();
      list.hidden = true;
      table.hidden = false;
      renderTable(table, sec, groups, actions, data.next[0]);
    }
  }
  player.refresh(all);
  if (fk) document.querySelector<HTMLElement>(`[data-fk="${CSS.escape(fk)}"]`)?.focus({ preventScroll: true });
  renderFeed();
}

function renderFeed(): void {
  const el = $("feed");
  const upd = data ? `Schedule fetched from Priyom at ${utcHM(ts(data.scheduleUpdated))} UTC.` : "";
  const how: Record<FeedMode, string> = {
    connecting: "Connecting for live updates.",
    live: "This page updates itself.",
    polling: "Checking for changes every minute.",
    offline: "Cannot reach the server; showing the last schedule received.",
  };
  el.textContent = `${upd} ${how[feedMode]}`.trim();
  if (!data && feedMode === "offline") {
    $("t-now").replaceChildren();
    el.textContent = "Cannot reach the server to load the schedule. It will retry every minute.";
  }
}

function tick(): void {
  const t = now();
  $("clock-utc").textContent = utcHMS(t);
  $("clock-local").textContent = localHMS(t);
  tickCountdowns(document);
  player.tick();
}

player.onChange = () => {
  render();
  const foot = $("foot-rx");
  const rx = document.getElementById("rx-link")?.textContent;
  const p = player.current;
  foot.textContent = p.state !== "idle" && rx ? `You are hearing ${rx}, ${$("rx-desc").textContent}.` : "";
};

// Free tune: any frequency the receivers cover, through the best one.
$("free-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const input = $("free-khz") as HTMLInputElement;
  const err = $("free-err");
  const khz = Number(input.value.trim().replace(",", "."));
  if (!Number.isFinite(khz) || khz < 10 || khz > 30000) {
    err.textContent = "Enter a frequency in kHz between 10 and 30000, for example 6070.";
    err.hidden = false;
    input.setAttribute("aria-invalid", "true");
    input.focus();
    return;
  }
  err.hidden = true;
  input.removeAttribute("aria-invalid");
  const mode = ($("free-mode") as HTMLSelectElement).value;
  player.open({ kind: "free", freqHz: Math.round(khz * 1000), mode });
});

$("later-toggle").addEventListener("click", () => {
  laterOpen = !laterOpen;
  try {
    localStorage.setItem(LATER_KEY, laterOpen ? "1" : "0");
  } catch {
    /* not remembered, still works */
  }
  render();
});

narrowMq.addEventListener("change", () => render());

$("clock-tz").textContent = localZone();
initReminders(render);
startFeed(
  (d) => {
    setServerTime(d.serverTime);
    data = d;
    render();
  },
  (m) => {
    feedMode = m;
    renderFeed();
  },
);
tick();
window.setInterval(tick, 1000);
