// The player: one channel at a time, with the idle connection policy from
// docs/brief.md enforced here on the browser side.
//
// - A channel is created only when the listener presses Play (or Listen on
//   the free tune form).
// - Pause ends the audio request (the fetch is aborted, or for the plain
//   <audio src> path the src is removed and load() called) and closes the
//   spectrum socket, then tells the server we left. Resume asks for the
//   channel again.
// - pagehide, a receiver switch, or opening something else closes the
//   current channel and sends a leave beacon.
// - A refused, failed or ended stream moves on to the next ranked receiver,
//   once through the list.

import type { ChannelResponse, ReceiverSummary, ScheduleEvent, SpectrumError, SpectrumHeader } from "./types";
import { $, h } from "./dom";
import { kHz, now, span, ts } from "./format";
import { AudioScope, RfWaterfall, loadHistory } from "./scopes";
import { LiveAudio, streamPath, type StreamFailure } from "./stream";
import { isNarrow, narrowMq, volumeWorks } from "./layout";

type State = "idle" | "connecting" | "playing" | "paused" | "failed";

/** What is being listened to: a scheduled transmission or a free tune. */
export type Target = { kind: "event"; ev: ScheduleEvent; freqHz: number } | { kind: "free"; freqHz: number; mode: string };

const START_TIMEOUT_MS = 15_000;
const HISTORY_REFRESH_MS = 60_000;

function sameTarget(a: Target | null, b: Target): boolean {
  if (!a || a.kind !== b.kind || a.freqHz !== b.freqHz) return false;
  if (a.kind === "event" && b.kind === "event") return a.ev.id === b.ev.id;
  return a.kind === "free" && b.kind === "free" && a.mode === b.mode;
}

export class Player {
  private readonly audio = $("audio") as HTMLAudioElement;
  private readonly live: LiveAudio;
  private actx: AudioContext | null = null;
  private gain: GainNode | null = null;
  private ch: ChannelResponse | null = null;
  private target: Target | null = null;
  private ws: WebSocket | null = null;
  private state: State = "idle";
  /** Receivers to fall back through, best first, fixed when the target opens. */
  private ranked: ReceiverSummary[] = [];
  private tried = new Set<string>();
  private gen = 0;
  private startTimer: number | undefined;
  private histTimer: number | undefined;
  private ourPause = false;
  private volume = 0.8;
  /** Set while falling back, so the note can say what happened once audio starts. */
  private fellBackFrom: string | null = null;
  /** One silent retry when the server says the channel has expired. */
  private rejoined = false;
  /**
   * Route audio through Web Audio for the spectrogram. Not where the OS owns
   * the volume (iOS): there a page's Web Audio is suspended when the screen
   * locks, which would silence the stream, and playing on with the screen
   * off matters more than the picture. ?webaudio=1 or 0 overrides.
   */
  private readonly useWebAudio: boolean;
  private sheetOpen = false;

  private readonly rf = new RfWaterfall($("rf") as HTMLCanvasElement, $("rf-marker"), $("rf-scale"), $("rf-pass"));
  private readonly af = new AudioScope($("af") as HTMLCanvasElement);

  /** Called whenever what is playing changes, so the schedule can mark it. */
  onChange: () => void = () => {};

  constructor() {
    let force: string | null = null;
    try {
      force = new URLSearchParams(location.search).get("webaudio");
    } catch {
      /* no location */
    }
    this.useWebAudio = force === "1" ? true : force === "0" ? false : volumeWorks;
    $("pl-vol-wrap").hidden = !volumeWorks;
    $("af-fig").hidden = !this.useWebAudio;
    $("player").dataset.path = streamPath();
    this.live = new LiveAudio(this.audio);
    this.rf.clear();
    const a = this.audio;
    a.addEventListener("playing", () => {
      if (this.state !== "connecting") return;
      window.clearTimeout(this.startTimer);
      if (this.fellBackFrom && this.ch) {
        const rx = this.ch.receiver;
        this.note(`${this.fellBackFrom}, so you are now hearing ${rx.callsign} in ${rx.location}.`);
        this.fellBackFrom = null;
      }
      this.setState("playing");
    });
    a.addEventListener("pause", () => {
      // Paused from outside the page (headset button, OS media controls):
      // treat it exactly like our own Pause so nothing is left open.
      if (a.ended || a.error) return;
      if (this.ourPause || (this.state !== "playing" && this.state !== "connecting")) return;
      this.pause();
    });

    $("pl-toggle").addEventListener("click", () => this.toggle());
    $("mini-toggle").addEventListener("click", () => this.toggle());
    $("pl-close").addEventListener("click", () => this.close());
    $("dock-toggle").addEventListener("click", () => this.toggle());
    $("dock-close").addEventListener("click", () => this.close());
    $("dock-open").addEventListener("click", () => this.expand());
    $("pl-collapse").addEventListener("click", () => this.collapse());
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && this.sheetOpen) this.collapse();
    });
    narrowMq.addEventListener("change", () => {
      if (!isNarrow() && this.sheetOpen) this.collapse();
      this.renderDock();
    });
    const vol = $("pl-vol") as HTMLInputElement;
    vol.value = String(this.volume);
    vol.addEventListener("input", () => {
      this.volume = Number(vol.value);
      if (this.gain) this.gain.gain.value = this.volume;
      else this.audio.volume = this.volume;
    });

    window.addEventListener("pagehide", () => {
      if (this.ch && (this.state === "playing" || this.state === "connecting")) {
        this.stopStreams();
        this.leave();
        this.setState("paused");
      }
    });

    if ("mediaSession" in navigator) {
      const ms = navigator.mediaSession;
      const set = (act: MediaSessionAction, f: () => void): void => {
        try {
          ms.setActionHandler(act, f);
        } catch {
          /* action not supported here */
        }
      };
      set("play", () => this.resume());
      set("pause", () => this.pause());
      set("stop", () => this.close());
    }
  }

  get current(): { eventId: string | null; freqHz: number; state: State } {
    const t = this.target;
    return { eventId: t?.kind === "event" ? t.ev.id : null, freqHz: t?.freqHz ?? 0, state: this.state };
  }

  /** Play a target. Must be called from the click handler. */
  open(t: Target): void {
    this.ensureAudio();
    if (sameTarget(this.target, t) && this.ch) {
      if (this.state === "paused" || this.state === "failed") this.resume();
      if (!isNarrow()) this.reveal();
      return;
    }
    if (this.ch) {
      this.stopStreams();
      this.leave();
    }
    this.target = t;
    this.ch = null;
    this.ranked = [];
    this.tried.clear();
    this.fellBackFrom = null;
    this.rejoined = false;
    this.note("");
    this.rf.clear();
    this.af.clear();
    $("hist-fig").hidden = true;
    this.renderHead();
    $("player").hidden = false;
    $("hint").hidden = true;
    // On a phone the schedule stays put; the dock at the bottom shows what
    // is playing and opens the full player.
    if (!isNarrow()) this.reveal();
    void this.connect(undefined);
  }

  /** Keep the player's copy of the event fresh (end times, signal). */
  refresh(events: ScheduleEvent[]): void {
    const t = this.target;
    if (t?.kind !== "event") return;
    const e = events.find((x) => x.id === t.ev.id);
    if (e) {
      t.ev = e;
      this.renderHead();
    }
  }

  tick(): void {
    const el = $("pl-left");
    const t = this.target;
    if (t?.kind !== "event") {
      el.textContent = "";
      return;
    }
    const n = now();
    const start = ts(t.ev.start);
    const end = ts(t.ev.end);
    if (n < start) el.textContent = `starts in ${span(start - n)}`;
    else if (n < end) el.textContent = `${span(end - n)} left${t.ev.endEstimated ? " (estimated)" : ""}`;
    else el.textContent = "scheduled end passed";
  }

  toggle(): void {
    if (this.state === "playing" || this.state === "connecting") this.pause();
    else this.resume();
  }

  pause(): void {
    if (!this.ch && this.state !== "connecting") return;
    this.stopStreams();
    this.leave();
    this.setState("paused");
  }

  resume(): void {
    if (!this.target) return;
    this.ensureAudio();
    if (this.state === "playing" || this.state === "connecting") return;
    this.tried.clear();
    this.fellBackFrom = null;
    this.rejoined = false;
    this.note("");
    void this.connect(this.ch?.receiver.key);
  }

  close(): void {
    if (this.sheetOpen) this.collapse();
    this.stopStreams();
    this.leave();
    this.ch = null;
    this.target = null;
    this.setState("idle");
    $("player").hidden = true;
    $("hint").hidden = false;
    if ("mediaSession" in navigator) navigator.mediaSession.metadata = null;
  }

  /** Phones: show the full player as a sheet over the page. */
  expand(): void {
    if (!this.target) return;
    if (!isNarrow()) {
      this.reveal();
      return;
    }
    const p = $("player");
    this.sheetOpen = true;
    p.classList.add("is-open");
    p.setAttribute("role", "dialog");
    p.setAttribute("aria-modal", "true");
    document.body.classList.add("sheet-open");
    for (const el of this.background()) el.inert = true;
    p.scrollTop = 0;
    $("pl-collapse").focus();
  }

  collapse(): void {
    const p = $("player");
    this.sheetOpen = false;
    p.classList.remove("is-open");
    p.removeAttribute("role");
    p.removeAttribute("aria-modal");
    document.body.classList.remove("sheet-open");
    for (const el of this.background()) el.inert = false;
    if (isNarrow() && !$("dock").hidden) $("dock-open").focus();
  }

  /** Everything the sheet covers. */
  private background(): HTMLElement[] {
    const out: HTMLElement[] = [];
    for (const el of Array.from(document.body.children)) {
      if (el.id === "main") {
        for (const c of Array.from(el.children)) if (c.id !== "player") out.push(c as HTMLElement);
      } else if (el.tagName !== "AUDIO" && el.tagName !== "SCRIPT") {
        out.push(el as HTMLElement);
      }
    }
    return out;
  }

  private renderDock(): void {
    const dock = $("dock");
    const t = this.target;
    const show = this.state !== "idle" && !!t;
    dock.hidden = !show;
    dock.dataset.state = this.state;
    document.body.classList.toggle("has-dock", show);
    if (!t) return;
    $("dock-what").textContent = t.kind === "event" ? `${t.ev.station} ${kHz(t.freqHz)} kHz` : `${kHz(t.freqHz)} kHz`;
    const mode = t.kind === "event" ? t.ev.priyomMode : t.mode.toUpperCase();
    const rx = this.ch?.receiver.callsign;
    const st = $("pl-state").textContent ?? "";
    $("dock-sub").textContent = [mode, rx ? `via ${rx}` : "", st].filter(Boolean).join(", ");
    const label = this.state === "playing" || this.state === "connecting" ? "Pause" : "Play";
    const b = $("dock-toggle");
    b.textContent = label;
    b.setAttribute("aria-label", `${label} ${this.label()}`.trim());
  }

  private switchTo(key: string): void {
    if (this.ch?.receiver.key === key && (this.state === "playing" || this.state === "connecting")) return;
    this.ensureAudio();
    this.stopStreams();
    this.leave();
    this.tried.clear();
    this.fellBackFrom = null;
    this.rejoined = false;
    this.note("");
    void this.connect(key);
  }

  private reveal(): void {
    const p = $("player");
    const r = p.getBoundingClientRect();
    if (r.top < 0 || r.top > window.innerHeight * 0.6) {
      const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      p.scrollIntoView({ behavior: reduce ? "auto" : "smooth", block: "start" });
    }
  }

  private ensureAudio(): void {
    // Created inside the user's click so browsers let it make sound.
    if (!this.useWebAudio) {
      this.audio.volume = this.volume;
      return;
    }
    if (!this.actx) {
      try {
        const ctx = new AudioContext();
        const src = ctx.createMediaElementSource(this.audio);
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 2048;
        analyser.smoothingTimeConstant = 0;
        analyser.minDecibels = -92;
        analyser.maxDecibels = -22;
        const gain = ctx.createGain();
        gain.gain.value = this.volume;
        src.connect(analyser);
        src.connect(gain).connect(ctx.destination);
        this.actx = ctx;
        this.gain = gain;
        this.af.attach(analyser);
      } catch {
        // No Web Audio: the <audio> element still plays on its own.
        this.audio.volume = this.volume;
      }
    }
    if (this.actx?.state === "suspended") void this.actx.resume();
  }

  private body(receiverKey: string | undefined): Record<string, unknown> {
    const t = this.target;
    if (!t) return {};
    if (t.kind === "event") return { eventId: t.ev.id, freqHz: t.freqHz, receiverKey };
    return { freqHz: t.freqHz, mode: t.mode, receiverKey };
  }

  private async connect(receiverKey: string | undefined): Promise<void> {
    if (!this.target) return;
    const gen = ++this.gen;
    this.setState("connecting");
    let res: Response;
    try {
      res = await fetch("/api/channels", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(this.body(receiverKey)),
      });
    } catch {
      if (gen !== this.gen) return;
      this.setState("failed");
      this.note("Could not reach this site's server. Check your connection, then press Play again.");
      return;
    }
    if (gen !== this.gen) return;
    if (!res.ok) {
      if (receiverKey && (res.status === 409 || res.status === 404) && this.ranked.length) {
        this.tried.add(receiverKey);
        this.fallback(`${this.nameOf(receiverKey)} cannot tune this frequency`);
        return;
      }
      this.setState("failed");
      if (this.target.kind === "free") {
        this.note(res.status === 409 || res.status === 404 ? "No receiver available to this site can tune that frequency." : `The server refused to open a channel (HTTP ${res.status}).`);
      } else {
        this.note(res.status === 404 ? "This transmission is no longer in the schedule." : `The server refused to open a channel (HTTP ${res.status}).`);
      }
      return;
    }
    const ch = (await res.json()) as ChannelResponse;
    if (gen !== this.gen) return;
    this.ch = ch;
    if (this.ranked.length === 0) this.ranked = [ch.receiver, ...ch.alternatives];
    this.renderHead();
    this.renderReceiver();
    this.startStreams(gen);
  }

  private startStreams(gen: number): void {
    const ch = this.ch;
    if (!ch) return;
    const q = `listener=${encodeURIComponent(ch.listenerId)}`;
    this.ourPause = false;
    this.live.start(`/listen/${encodeURIComponent(ch.channelId)}/audio.webm?${q}`, {
      failed: (f) => {
        if (gen === this.gen) this.streamFailed(f);
      },
    });
    this.audio.play().catch((err: unknown) => {
      if (gen !== this.gen) return;
      if (err instanceof DOMException && err.name === "NotAllowedError") {
        this.stopStreams();
        this.leave();
        this.setState("paused");
        this.note("Your browser blocked playback. Press Play to listen.");
      }
      // AbortError and friends: a later src change or the stream handler deals with it.
    });
    window.clearTimeout(this.startTimer);
    this.startTimer = window.setTimeout(() => {
      if (gen === this.gen && this.state === "connecting") this.streamFailed({ kind: "network", detail: "timeout" });
    }, START_TIMEOUT_MS);

    if (ch.capabilities.liveSpectrum) this.openSpectrum(ch, gen);
    $("rf-fig").hidden = !ch.capabilities.liveSpectrum;
    this.af.start();
    this.loadHistory(ch);
    this.updateMediaSession();
  }

  private openSpectrum(ch: ChannelResponse, gen: number): void {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const url = `${proto}//${location.host}/listen/${encodeURIComponent(ch.channelId)}/spectrum?listener=${encodeURIComponent(ch.listenerId)}`;
    const ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    const status = $("rf-status");
    status.textContent = "Waiting for the waterfall";
    status.hidden = false;
    ws.onmessage = (m: MessageEvent) => {
      if (gen !== this.gen) return;
      if (typeof m.data === "string") {
        let msg: SpectrumHeader | SpectrumError;
        try {
          msg = JSON.parse(m.data) as SpectrumHeader | SpectrumError;
        } catch {
          return;
        }
        if (msg.type === "header") {
          this.rf.setHeader(msg, ch.mode, ch.freqHz);
          status.hidden = true;
        } else if (msg.type === "error") {
          status.textContent = `Waterfall stopped: ${msg.reason ?? msg.error}`;
          status.hidden = false;
        }
        return;
      }
      this.rf.push(new Uint8Array(m.data as ArrayBuffer));
    };
    ws.onclose = () => {
      if (gen !== this.gen || this.ws !== ws) return;
      this.ws = null;
      if (this.state === "playing" || this.state === "connecting") {
        if (status.hidden) status.textContent = "Waterfall stopped. Audio carries on.";
        status.hidden = false;
      }
    };
    this.ws = ws;
  }

  private loadHistory(ch: ChannelResponse): void {
    const fig = $("hist-fig");
    window.clearTimeout(this.histTimer);
    if (!ch.capabilities.historicalSpectrogram) {
      fig.hidden = true;
      return;
    }
    const url = `/listen/${encodeURIComponent(ch.channelId)}/spectrogram.png?minutes=30`;
    const id = ch.channelId;
    const load = (): void => {
      void loadHistory(url, $("hist") as HTMLCanvasElement).then((ok) => {
        if (this.ch?.channelId !== id) return;
        fig.hidden = !ok;
        if (ok && (this.state === "playing" || this.state === "connecting")) {
          this.histTimer = window.setTimeout(load, HISTORY_REFRESH_MS);
        }
      });
    };
    load();
  }

  /** Ends the audio request and closes the spectrum socket. */
  private stopStreams(): void {
    this.gen++;
    window.clearTimeout(this.startTimer);
    window.clearTimeout(this.histTimer);
    this.ourPause = true;
    this.live.stop();
    if (this.ws) {
      const ws = this.ws;
      this.ws = null;
      ws.onmessage = null;
      ws.onclose = null;
      ws.close(1000, "listener left");
    }
    this.af.stop();
    $("rf-status").hidden = true;
  }

  /** Best-effort explicit leave; the server never depends on it. */
  private leave(): void {
    const ch = this.ch;
    if (!ch) return;
    const url = `/api/channels/${encodeURIComponent(ch.channelId)}/listeners/${encodeURIComponent(ch.listenerId)}/leave`;
    try {
      if (!navigator.sendBeacon(url)) void fetch(url, { method: "POST", keepalive: true }).catch(() => {});
    } catch {
      /* nothing more we can do */
    }
  }

  private streamFailed(f: StreamFailure): void {
    const ch = this.ch;
    if (!ch || (this.state !== "connecting" && this.state !== "playing")) return;
    const call = ch.receiver.callsign;
    this.stopStreams();
    this.leave();
    if (f.kind === "unsupported") {
      // A browser limitation, not a receiver fault: trying others will not help.
      this.setState("failed");
      const link = ch.receiver.deepLink;
      this.note(`This browser cannot play the audio stream (WebM with Opus).${link ? " You can still listen on the receiver's own page, linked below." : ""}`);
      return;
    }
    if (f.kind === "http" && f.error === "no_channel" && !this.rejoined) {
      // The server forgot the channel (a restart, say): ask for it again.
      this.rejoined = true;
      void this.connect(ch.receiver.key);
      return;
    }
    this.tried.add(ch.receiver.key);
    this.fallback(describe(call, f));
  }

  private fallback(what: string): void {
    const next = this.ranked.find((r) => !this.tried.has(r.key));
    if (!next) {
      this.fellBackFrom = null;
      this.setState("failed");
      const link = this.ch?.receiver.deepLink ?? this.ranked[0]?.deepLink;
      this.note(`${what}. No other receiver is left to try. Press Play to start again${link ? ", or open the receiver's own page" : ""}.`);
      this.renderReceiver();
      return;
    }
    this.fellBackFrom = what;
    this.note(`${what}. Trying ${next.callsign} in ${next.location} instead.`);
    void this.connect(next.key);
  }

  private nameOf(key: string): string {
    return this.ranked.find((r) => r.key === key)?.callsign ?? "That receiver";
  }

  private label(): string {
    const t = this.target;
    if (!t) return "";
    return t.kind === "event" ? `${t.ev.station} on ${kHz(t.freqHz)} kHz` : `${kHz(t.freqHz)} kHz`;
  }

  private setState(s: State): void {
    this.state = s;
    const label = s === "playing" || s === "connecting" ? "Pause" : "Play";
    for (const id of ["pl-toggle", "mini-toggle"]) {
      const b = $(id);
      b.textContent = label;
      b.setAttribute("aria-label", `${label} ${this.label()}`.trim());
    }
    const stateText: Record<State, string> = {
      idle: "",
      connecting: "Connecting",
      playing: "Listening",
      paused: "Paused",
      failed: "Stopped",
    };
    $("pl-state").textContent = stateText[s];
    $("player").dataset.state = s;
    const mini = $("mini");
    mini.hidden = s === "idle";
    mini.dataset.state = s;
    const t = this.target;
    if (t) $("mini-what").textContent = t.kind === "event" ? `${t.ev.station} ${kHz(t.freqHz)} kHz` : `${kHz(t.freqHz)} kHz ${t.mode.toUpperCase()}`;
    if ("mediaSession" in navigator) {
      navigator.mediaSession.playbackState = s === "playing" ? "playing" : s === "idle" ? "none" : "paused";
    }
    this.renderDock();
    this.onChange();
  }

  private note(text: string): void {
    const n = $("pl-note");
    n.textContent = text;
    n.hidden = text === "";
  }

  private renderHead(): void {
    const t = this.target;
    if (!t) return;
    const pri = $("pl-priyom") as HTMLAnchorElement;
    const tgt = $("pl-target");
    const dig = $("pl-digital");
    $("pl-freq").textContent = kHz(t.freqHz);
    $("pl-des").classList.toggle("mono", t.kind === "event");
    if (t.kind === "event") {
      const ev = t.ev;
      $("pl-des").textContent = ev.station;
      $("pl-name").textContent = ev.stationName || "";
      $("pl-mode").textContent = ev.priyomMode + (ev.digital ? " (data)" : "");
      tgt.textContent = ev.target ? `Target: ${ev.target}` : "";
      tgt.hidden = !ev.target;
      if (ev.priyomUrl) {
        pri.href = ev.priyomUrl;
        pri.hidden = false;
      } else {
        pri.hidden = true;
      }
      dig.hidden = !ev.digital;
      if (ev.digital) {
        const dial = this.ch?.tunedHz;
        const where = dial && dial !== t.freqHz ? ` The receiver is tuned to ${kHz(dial)} kHz USB, so the tones sit in the middle of what you hear.` : "";
        dig.textContent = `A data mode: you will hear the raw tones, and decoding needs separate software.${where}`;
      }
    } else {
      $("pl-des").textContent = "Free tune";
      $("pl-name").textContent = "";
      $("pl-mode").textContent = t.mode.toUpperCase();
      tgt.hidden = true;
      pri.hidden = true;
      dig.hidden = true;
    }
    this.tick();
  }

  private renderReceiver(): void {
    const ch = this.ch;
    if (!ch) return;
    const rx = ch.receiver;
    const link = $("rx-link") as HTMLAnchorElement;
    link.href = rx.publicUrl;
    link.textContent = rx.callsign;
    $("rx-desc").textContent = `${rx.name}, ${rx.location}`;
    const deep = $("rx-deep") as HTMLAnchorElement;
    if (rx.deepLink) {
      deep.href = rx.deepLink;
      deep.textContent = `Open ${rx.callsign}'s own receiver page, already tuned`;
      deep.hidden = false;
    } else {
      deep.hidden = true;
    }
    const top = this.ranked.slice(0, 3);
    if (!top.some((r) => r.key === rx.key)) top.push(rx);
    $("rx-switch").hidden = top.length < 2;
    this.renderDock();
    $("rx-list").replaceChildren(
      ...top.map((r) => {
        const cur = r.key === rx.key;
        return h(
          "li",
          null,
          h(
            "button",
            {
              type: "button",
              class: "rx-opt",
              "aria-pressed": cur ? "true" : "false",
              onclick: () => this.switchTo(r.key),
            },
            h("span", { class: "rx-call" }, r.callsign),
            h("span", { class: "rx-loc" }, r.location),
            h("span", { class: "rx-dist" }, typeof r.distanceKm === "number" ? `${Math.round(r.distanceKm).toLocaleString("en-GB")} km` : ""),
            this.tried.has(r.key) && !cur ? h("span", { class: "rx-failed" }, "did not work") : null,
          ),
        );
      }),
    );
    this.onChange();
  }

  private updateMediaSession(): void {
    const t = this.target;
    if (!("mediaSession" in navigator) || !t || !this.ch) return;
    const rx = this.ch.receiver;
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: t.kind === "event" ? `${t.ev.station}${t.ev.stationName ? ` ${t.ev.stationName}` : ""}` : `${kHz(t.freqHz)} kHz`,
        artist: t.kind === "event" ? `${kHz(t.freqHz)} kHz ${t.ev.priyomMode}` : t.mode.toUpperCase(),
        album: `Receiver ${rx.callsign}, ${rx.location}`,
        artwork: [
          { src: "/icon-192.png", sizes: "192x192", type: "image/png" },
          { src: "/icon-512.png", sizes: "512x512", type: "image/png" },
        ],
      });
    } catch {
      /* MediaMetadata missing */
    }
  }
}

/** Plain-language reason a receiver's stream did not work. */
function describe(call: string, f: StreamFailure): string {
  switch (f.kind) {
    case "http":
      switch (f.error) {
        case "receiver_busy":
          return `${call} has no free slot for this site right now`;
        case "rejected":
          return `${call} turned the connection down${f.reason ? ` (${f.reason})` : ""}`;
        case "upstream":
          return `${call} could not be reached${f.reason ? ` (${f.reason})` : ""}`;
        case "no_channel":
          return `The channel on ${call} expired`;
        default:
          return `${call} could not be used (HTTP ${f.status})`;
      }
    case "eof":
      return `${call} ended the session (it may have reached its time limit)`;
    case "network":
      return f.detail === "timeout" ? `${call} did not start sending audio` : `The connection to ${call} dropped`;
    case "media":
      return `The audio from ${call} could not be played`;
    case "unsupported":
      return "This browser cannot play the stream";
  }
}
