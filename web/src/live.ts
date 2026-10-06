// Schedule updates: Server-Sent Events from /api/live, falling back to
// polling /api/now every 60 s when the stream cannot be held open. While
// polling, the stream is retried every 5 minutes.

import type { NowResponse } from "./types";

const POLL_MS = 60_000;
const SSE_RETRY_MS = 5 * 60_000;
const MAX_SSE_ERRORS = 3;

export type FeedMode = "connecting" | "live" | "polling" | "offline";

export function startFeed(onData: (d: NowResponse) => void, onMode: (m: FeedMode) => void): void {
  let es: EventSource | null = null;
  let pollTimer: number | undefined;
  let retryTimer: number | undefined;
  let errors = 0;

  const poll = async (): Promise<void> => {
    try {
      const res = await fetch("/api/now", { cache: "no-store" });
      if (!res.ok) throw new Error(String(res.status));
      onData((await res.json()) as NowResponse);
      onMode("polling");
    } catch {
      onMode("offline");
    }
  };

  const startPolling = (): void => {
    if (pollTimer !== undefined) return;
    void poll();
    pollTimer = window.setInterval(() => void poll(), POLL_MS);
    window.clearTimeout(retryTimer);
    retryTimer = window.setTimeout(openStream, SSE_RETRY_MS);
  };

  const stopPolling = (): void => {
    window.clearInterval(pollTimer);
    pollTimer = undefined;
    window.clearTimeout(retryTimer);
  };

  function openStream(): void {
    if (typeof EventSource !== "function") {
      startPolling();
      return;
    }
    errors = 0;
    es = new EventSource("/api/live");
    es.addEventListener("open", () => {
      errors = 0;
      stopPolling();
      onMode("live");
    });
    es.addEventListener("now", (e: MessageEvent) => {
      try {
        onData(JSON.parse(e.data as string) as NowResponse);
      } catch {
        /* a bad frame; the next one will do */
      }
    });
    es.addEventListener("error", () => {
      errors++;
      // CLOSED means the browser gave up (an HTTP error, say). Repeated
      // failures while reconnecting get the same treatment.
      if (es && (es.readyState === EventSource.CLOSED || errors >= MAX_SSE_ERRORS)) {
        es.close();
        es = null;
        startPolling();
      }
    });
  }

  // First paint should not wait for the stream to open.
  onMode("connecting");
  void fetch("/api/now", { cache: "no-store" })
    .then((r) => (r.ok ? (r.json() as Promise<NowResponse>) : null))
    .then((d) => d && onData(d))
    .catch(() => {});
  openStream();
}
