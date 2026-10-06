// Live audio into the page's <audio> element.
//
// Preferred path: fetch() the WebM/Opus stream and feed it to a MediaSource
// (ManagedMediaSource on iOS Safari). Browsers hold back about 32 KB before
// starting a plain <audio src> on a live WebM, which at these bit rates is
// about 6 s late and stays that far behind; through MSE playback starts
// after the first cluster and runs under a second behind. Fetching also lets
// us read the server's JSON error body.
//
// Fallback: plain <audio src> where MSE cannot play WebM/Opus.
//
// stop() aborts the fetch (or removes the src and calls load()), which is
// what ends the listener's HTTP request and so releases the relay.

export type StreamFailure =
  | { kind: "http"; status: number; error: string; reason?: string }
  | { kind: "eof" }
  | { kind: "network"; detail: string }
  | { kind: "media"; detail: string };

export interface StreamHandlers {
  failed(f: StreamFailure): void;
}

const TYPE = 'audio/webm; codecs="opus"';
/** Jump forward when playback falls this far behind the newest audio. */
const MAX_LAG_S = 3;
/** Where to land after a jump, measured back from the newest audio. */
const JUMP_TO_S = 0.5;
/** Keep this much already-played audio; drop anything older. */
const KEEP_S = 30;

type MSCtor = typeof MediaSource;

function mediaSourceCtor(): { ctor: MSCtor; managed: boolean } | null {
  try {
    if (new URLSearchParams(location.search).get("mse") === "0") return null; // debugging switch
  } catch {
    /* no location */
  }
  const w = window as unknown as { MediaSource?: MSCtor; ManagedMediaSource?: MSCtor };
  if (w.MediaSource && w.MediaSource.isTypeSupported(TYPE)) return { ctor: w.MediaSource, managed: false };
  if (w.ManagedMediaSource && w.ManagedMediaSource.isTypeSupported(TYPE)) return { ctor: w.ManagedMediaSource, managed: true };
  return null;
}

export function usesMse(): boolean {
  return mediaSourceCtor() !== null;
}

export class LiveAudio {
  private ctrl: AbortController | null = null;
  private objectUrl: string | null = null;
  private ms: MediaSource | null = null;
  private active = false;
  private handlers: StreamHandlers | null = null;
  private readonly onError = (): void => {
    const e = this.audio.error;
    this.fail({ kind: "media", detail: e ? `media error ${e.code}${e.message ? `: ${e.message}` : ""}` : "media error" });
  };
  private readonly onEnded = (): void => this.fail({ kind: "eof" });

  constructor(private readonly audio: HTMLAudioElement) {}

  /** Starts streaming url into the element. play() is the caller's job. */
  start(url: string, handlers: StreamHandlers): void {
    this.stop();
    this.active = true;
    this.handlers = handlers;
    this.audio.addEventListener("error", this.onError);
    this.audio.addEventListener("ended", this.onEnded);
    const m = mediaSourceCtor();
    if (m) this.startMse(url, m.ctor, m.managed);
    else this.audio.src = url;
  }

  /** Ends the HTTP request and detaches everything from the element. */
  stop(): void {
    this.active = false;
    this.handlers = null;
    this.audio.removeEventListener("error", this.onError);
    this.audio.removeEventListener("ended", this.onEnded);
    this.ctrl?.abort();
    this.ctrl = null;
    this.audio.pause();
    this.audio.removeAttribute("src");
    this.audio.load(); // for the plain path, this is what aborts the request
    if (this.objectUrl) URL.revokeObjectURL(this.objectUrl);
    this.objectUrl = null;
    this.ms = null;
  }

  private fail(f: StreamFailure): void {
    if (!this.active) return;
    const h = this.handlers;
    this.stop();
    h?.failed(f);
  }

  private startMse(url: string, Ctor: MSCtor, managed: boolean): void {
    const ms = new Ctor();
    this.ms = ms;
    const ctrl = new AbortController();
    this.ctrl = ctrl;
    if (managed) {
      // ManagedMediaSource only attaches when remote playback is off.
      (this.audio as HTMLAudioElement & { disableRemotePlayback: boolean }).disableRemotePlayback = true;
    }
    this.objectUrl = URL.createObjectURL(ms);
    this.audio.src = this.objectUrl;
    ms.addEventListener("sourceopen", () => void this.pump(url, ms, ctrl), { once: true });
  }

  private async pump(url: string, ms: MediaSource, ctrl: AbortController): Promise<void> {
    if (!this.active || this.ms !== ms) return;
    let sb: SourceBuffer;
    try {
      sb = ms.addSourceBuffer(TYPE);
      sb.mode = "sequence";
    } catch (e) {
      this.fail({ kind: "media", detail: `cannot play WebM/Opus here (${String(e)})` });
      return;
    }
    const idle = (): Promise<void> =>
      sb.updating ? new Promise((r) => sb.addEventListener("updateend", () => r(), { once: true })) : Promise.resolve();

    let res: Response;
    try {
      res = await fetch(url, { signal: ctrl.signal, cache: "no-store" });
    } catch (e) {
      if (!ctrl.signal.aborted) this.fail({ kind: "network", detail: String(e) });
      return;
    }
    if (!res.ok || !res.body) {
      let error = `http_${res.status}`;
      let reason: string | undefined;
      try {
        const j = (await res.json()) as { error?: string; reason?: string };
        if (j.error) error = j.error;
        reason = j.reason;
      } catch {
        /* not JSON */
      }
      if (!ctrl.signal.aborted) this.fail({ kind: "http", status: res.status, error, reason });
      return;
    }

    const reader = res.body.getReader();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (ctrl.signal.aborted || this.ms !== ms) return;
        if (done) {
          this.fail({ kind: "eof" });
          return;
        }
        if (!value || value.byteLength === 0) continue;
        await idle();
        if (ms.readyState !== "open") return;
        try {
          sb.appendBuffer(value);
        } catch (e) {
          if (e instanceof DOMException && e.name === "QuotaExceededError") {
            // Make room and try once more.
            await this.trim(sb, idle, 5);
            sb.appendBuffer(value);
          } else {
            throw e;
          }
        }
        await idle();
        this.keepLive(sb);
        await this.trim(sb, idle, KEEP_S);
      }
    } catch (e) {
      if (ctrl.signal.aborted || this.ms !== ms) return;
      this.fail({ kind: "network", detail: String(e) });
    } finally {
      reader.cancel().catch(() => {});
    }
  }

  /** Stay near the newest audio; a stall or a background tab can leave us behind. */
  private keepLive(sb: SourceBuffer): void {
    const b = sb.buffered;
    if (b.length === 0) return;
    const end = b.end(b.length - 1);
    const a = this.audio;
    if (a.paused && a.readyState < 2) return;
    if (end - a.currentTime > MAX_LAG_S) {
      a.currentTime = Math.max(b.start(b.length - 1), end - JUMP_TO_S);
    }
  }

  private async trim(sb: SourceBuffer, idle: () => Promise<void>, keep: number): Promise<void> {
    const b = sb.buffered;
    if (b.length === 0) return;
    const cut = this.audio.currentTime - keep;
    if (cut > b.start(0) + 1) {
      await idle();
      try {
        sb.remove(b.start(0), cut);
      } catch {
        return;
      }
      await idle();
    }
  }
}
