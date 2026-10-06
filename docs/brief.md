# Numbers Station Listener — Build Brief

A website that makes it easy to listen to numbers stations. It shows what is **on now** and **up next** from the Priyom schedule. It picks the best public SDR for each transmission and plays it **in-page** with audio, an RF waterfall and a spectrogram.

Research notes verified 2026-10-06. Treat anything marked **VERIFY** as unconfirmed.

---

## Decisions already made

- **Own site**, not a contribution to Priyom or UberSDR.
- **In-page listening**: audio, live RF waterfall and spectrogram. No bouncing users to the receiver's own UI, though a deep link is kept as a fallback.
- **UberSDR only** for now. Leave a clean seam for a **KiwiSDR** provider later.
- **Nathan (madpsy, M9PSY), the UberSDR author, has OK'd building this on top of UberSDR**, relay included (confirmed 2026-10-06).
- **Never hold an UberSDR connection nobody is listening to.** See "Idle connection policy"; this is a hard requirement.
- Stack: **.NET 10 / ASP.NET Core** backend, **TypeScript + Vite** frontend (no heavy framework unless it earns its place). Ship as a single container. Config comes from environment variables. Hosting is TBD and Tom will decide.

---

## Data sources

### 1. Priyom schedule (undocumented JSON feed)

```
GET https://calendar.priyom.org/events?timeMin=<ISO8601>&timeMax=<ISO8601>
→ { "items": [ { "summary": "V13 15388kHz USB/AM [Target: East Asia]",
                 "start": { "dateTime": "2026-10-07T00:00:00.000Z" } }, ... ] }
```

- Found in Priyom's own viewer, [priyom/calendar-web-viewer `src/fetch-priyom.js`](https://github.com/priyom/calendar-web-viewer/blob/master/src/fetch-priyom.js). It fetches one UTC day per request.
- The viewer parses `summary` with this regex:
  `/^(\w+) (Search|([0-9].*kHz) ([A-Z/]+)) ?(.*)/`. That yields station, frequencies (comma-separated, kHz), mode and remarks. Write a more forgiving parser: log and keep unparsed items rather than dropping them, and handle multi-frequency entries, `Search` entries (no frequency) and bracketed remarks such as `[Target: …]`.
- There are **start times only, no end times or durations.** Durations come from the station catalogue (below), refined by live signal detection.
- Licence: priyom.org content is **CC BY-NC-SA 4.0**. Attribute Priyom visibly and link to station pages. The site must stay non-commercial.
- Poll gently: today plus tomorrow (UTC) every ~15 min, cached server-side. Never call it per page view.
- Tom will contact the Priyom team (IRC #priyom on Libera, or Discord) about using the feed.

### 2. UberSDR directory

```
GET https://instances.ubersdr.org/api/instances?online_only=true
→ { "count": 53, "instances": [ { id, callsign, name, location, latitude, longitude,
     maidenhead, country_code, public_url, host, port, tls, version, load_status,
     max_clients, available_clients, max_session_time, cors_enabled,
     snr_0_30_mhz, snr_1_8_30_mhz, is_daylight, noise_floor, digital_decodes,
     cw_skimmer, tuning_range{min_frequency,max_frequency}, antenna_connected,
     last_report_age_seconds, is_online, ... } ] }
```

- At the time of writing there were 53 instances online. They are **Europe-heavy**: a sample of 36 had 12 in GB, 5 in US, 4 in AT and the rest spread across Western Europe. Expect weak coverage for stations aimed at Asia or the Pacific.
- **`cors_enabled` was `false` on every instance sampled.** See the CORS section below; this drives the architecture.
- `GET /api/instances/{uuid}` returns one instance.
- Tom runs an instance himself (callsign **M0LTE**, "SDR with Active Loop", Reading). **Use it as the dev and integration target.** Don't hammer public instances during development.

### 3. UberSDR instance API (per receiver)

Source: [madpsy/ka9q_ubersdr](https://github.com/madpsy/ka9q_ubersdr), **GPL-3.0**. Read it for the protocol. Do **not** copy code into this project unless the project is GPL-compatible.

| Endpoint | Notes |
|---|---|
| `POST /connection` body `{"user_session_id": "<uuid>", "password"?: "..."}` | Must be called before `/ws` will accept the session. Returns `{allowed, reason, max_session_time, allowed_iq_modes, ...}`; 403 means a bad password. Rate-limited per IP. |
| `WS /ws?frequency=<Hz>&mode=<m>&user_session_id=<uuid>&bandwidthLow=&bandwidthHigh=&format=opus\|pcm-zstd&version=<n>` | Audio session. Reference: `clients/python/radio_client.py` (URL build ~L735, `/connection` ~L1771) and `static/app.js`. |
| `WS /ws/user-spectrum` | Per-user spectrum/waterfall feed. Wire format is in `user_spectrum_websocket.go`, `user_spectrum_v2*.go` and their tests. |
| `GET /audio/stream?session=<id>` | WebM/Opus HTTP stream of an **existing** WS session (see `audio_http_stream.go`). A useful reference for our own WebM muxing. |
| `GET /api/spectrogram`, `/api/spectrogram/latest`, `/timeslice`, `/meta`, `/thumb` … | Recorded wideband spectrogram as PNG, one file per UTC day. Query params include `freq_min`, `freq_max`, `rolling=1`, `date`, `palette`, `db_min`, `db_max`, `band`. Rate-limited to **1 PNG per 10 s per IP**. See `spectrogram_recorder.go`. |
| `GET /api/noisefloor/fft`, `/api/noisefloor/fft/wideband` | Rate-limited (default 1 per 2 s per IP). Candidate for signal probing. **VERIFY** the resolution is good enough. |
| Deep link: `<public_url>?freq=<Hz>&mode=usb\|lsb\|cwu\|cwl\|am\|sam\|fm\|nfm` | Opens the receiver's own UI already tuned. Frequency must be 10 000–30 000 000 Hz. This is the fallback path. |

The websocket upgrader accepts any Origin, but `POST /connection` is a JSON POST. In a browser that triggers a preflight, and the preflight fails unless the instance has `enable_cors` on. The only exceptions are the directory host and loopback origins (see the comments in `main.go` around the CORS block and `cors_loopback_test.go`).

### 4. PSKReporter MQTT (later milestone)

`mqtt.pskreporter.info` publishes a live spot feed. Numbers stations never appear in it, so use it only as a **path-open heuristic**: spots from near the station's transmitter site to near a candidate receiver, on amateur bands close to the station's frequency. **VERIFY** the topic structure (believed to be `pskr/filter/v2/{band}/{mode}/{sendercall}/{receivercall}/{senderlocator}/{receiverlocator}/{sendercountry}/{receivercountry}`).

---

## The CORS constraint and why we relay

With CORS off everywhere, a page on our domain **cannot** open an UberSDR session directly from the browser. Do **not** work around this with `no-cors` or a `text/plain` POST. Operators control that setting, and the directory presents it as an opt-in feature.

Instead the backend runs a **relay**:

- The server opens the upstream session (`/connection` then `/ws` and the spectrum websocket). Server-to-server traffic has no CORS restriction.
- **Fan-out:** at most one upstream session per (provider, receiver, frequency, mode), shared by every listener on our site. This uses fewer receiver slots than sending each user to the SDR directly.
- Browsers only ever talk to our origin. That makes audio analysis (`AnalyserNode`) same-origin and straightforward.

Consequences to design for:

- All upstream traffic comes from **one IP**. Instance per-IP limits will apply to all our listeners combined: `/connection` rate limits, connection rate limits, `max_daily_time_per_ip`, and the FFT and spectrogram rate limits. Cache aggressively and handle rejections gracefully by trying the next receiver.
- Operators see our relay rather than individual listeners. The UberSDR author has approved this approach, but in return we must be scrupulous about slots: hold no idle connections, keep fan-out and per-receiver caps, and attribute the receiver clearly.
- Keep a second path in the design: **browser-direct** for instances that have `cors_enabled: true`. Implement it only if any appear; Tom can enable CORS on M0LTE to test it.

---

## Architecture

### Backend (ASP.NET Core)

Hosted services:

1. **ScheduleService** polls Priyom every 15 min for today and tomorrow (UTC). It parses events, assigns a stable `eventId` (hash of start, station and frequencies), and stores them in memory with an optional SQLite snapshot for restarts.
2. **DirectoryService** asks each provider for its receivers every ~5 min.
3. **StationCatalog** is a hand-curated `stations.json` in the repo (see below).
4. **Ranker** scores receivers for each event (see below).
5. **Prober** checks whether the signal is actually present. It runs from 60 s before start, then about every 60 s while the event is "live", on the top K (≈5) candidates. Results are cached and shared by all users; the probe count must stay independent of listener count.
6. **RelayManager** owns refcounted upstream sessions and enforces the idle connection policy below. It caps upstream sessions per receiver (default 2) and sends a descriptive `User-Agent` with the site URL and a contact address.

### Idle connection policy (hard requirement)

An upstream UberSDR connection (`/ws`, `/ws/user-spectrum`) may exist **only while at least one browser is actively listening to that channel**.

- **What counts as a listener.** Only an open audio stream (`/listen/{id}/audio.webm`) or spectrum WS for that channel counts. Viewing the schedule, the SSE feed or the spectrogram history strip does **not**.
- **Release on zero.** When a channel's listener count drops to 0, close every upstream socket for it after a short grace period. The default is **10 s** (configurable, never more than 30 s), which absorbs page reloads and receiver switches. Close the sockets cleanly with a WebSocket close frame.
- **Detect dead listeners quickly.**
  - Treat a failed write or `RequestAborted` on the audio stream as a disconnect.
  - Ping the spectrum WS and drop it after **15 s** with no pong.
  - Never wait for TCP timeouts.
- **Frontend releases explicitly.**
  - **Pause** closes both the audio stream and the spectrum WS, so a paused listener is not listening. Resuming creates or rejoins the channel.
  - On `pagehide`, receiver switch, or opening a different event, close the current channel. Send a best-effort `navigator.sendBeacon` to `DELETE /api/channels/{id}/listeners/{listenerId}`.
  - Background tabs that are still playing are legitimate listeners; keep them.
- **No pre-warming.** Never open an upstream session before a user presses Play: not for "up next", not ahead of the start time, not to speed up switching.
- **Session expiry.** When the receiver's `max_session_time` expires, reconnect **only if** listeners remain; otherwise let the session go.
- **Probes hold nothing.** Prefer HTTP endpoints for probing. If a probe needs a brief spectrum session, open it, measure, and close it in the same operation, with a hard timeout of 10 s.
- **Watchdog.** A sweep every 5 s force-closes any upstream socket whose channel has had 0 listeners for longer than the grace period. It logs this as a bug, because it means a refcount leak.
- **Visibility.** Expose upstream sessions alongside listener counts per channel (`/admin/sessions` plus metrics). Alert if any upstream session has had 0 listeners for longer than the grace period.

### Provider seam (leave room for KiwiSDR)

```csharp
interface IReceiverProvider {
  string Id { get; }                                    // "ubersdr", later "kiwisdr"
  Task<IReadOnlyList<Receiver>> ListAsync(CancellationToken ct);
  Task<ProbeResult> ProbeAsync(Receiver r, long freqHz, CancellationToken ct);
  Task<IUpstreamSession> OpenAsync(Receiver r, long freqHz, Mode mode, int spanHz, CancellationToken ct);
  Uri DeepLink(Receiver r, long freqHz, Mode mode);
  ProviderCapabilities Capabilities { get; }            // e.g. HistoricalSpectrogram (UberSDR yes, Kiwi no)
}
interface IUpstreamSession : IAsyncDisposable {
  IAsyncEnumerable<AudioPacket> Audio { get; }          // normalised: Opus packets + sample rate
  IAsyncEnumerable<SpectrumRow> Spectrum { get; }       // normalised: startHz, binHz, byte[] dB-scaled
}
```

- Everything downstream of the provider (relay, API, frontend) is provider-agnostic.
- KiwiSDR notes for later: directory via `rx.linkfanel.net` (Priyom's receiver map) or the kiwisdr.com public list. Audio arrives as PCM/ADPCM, so it would need Opus encoding (e.g. Concentus). It provides a waterfall but no historical spectrogram.

### API exposed to the browser

| Route | Purpose |
|---|---|
| `GET /api/schedule?from=&to=` | Events with derived fields: status, best receiver, signal state. |
| `GET /api/now` | On now / up next / later today. |
| `GET /api/live` (SSE) | Push updates for status changes, probe results and receiver changes. |
| `POST /api/channels` `{eventId}` or `{freqHz, mode, receiverId?}` | Get-or-create a relay channel and return `channelId` and receiver info. |
| `DELETE /api/channels/{channelId}/listeners/{listenerId}` | Explicit leave, sent via `sendBeacon` on pagehide or switch. It speeds up release; the server must not depend on it. |
| `GET /listen/{channelId}/audio.webm` | Live WebM/Opus stream. Remux the upstream Opus; no transcoding. |
| `WS /listen/{channelId}/spectrum` | JSON header `{centerHz, spanHz, bins}`, then binary rows. |
| `GET /listen/{channelId}/spectrogram.png?minutes=30` | Cached proxy of the instance's recorded spectrogram around the frequency. Respect the 1-per-10 s upstream limit through a shared cache. |

### Frontend

- **Schedule view**: On now / Up next / Later today, with UTC and local times and live countdowns. Each card shows station, frequency, mode, target remarks and a link to the Priyom page. Badges show "signal detected" (from probes) and the receiver name and location.
- **Player** (per event, one at a time):
  - **Audio**: an `<audio>` element pointed at `audio.webm` (broad browser support, plus lock-screen controls via `navigator.mediaSession` with station metadata).
  - **Audio spectrogram**: `MediaElementAudioSourceNode` → `AnalyserNode` → scrolling canvas.
  - **RF waterfall**: canvas fed by the spectrum WS, zoomed to about ±5–10 kHz around the frequency, with a marker at the tuned frequency.
  - **History strip**: the last ~30 min of the instance spectrogram around the frequency, which answers "did it already start?". Hide it when the provider lacks the capability.
  - **Receiver switcher**: the top 3 ranked receivers, plus a deep link to open the current one in its own UI.
  - **Attribution**: receiver callsign, name and location with a link to `public_url`. Priyom attribution goes in the footer and on each card.
- **Reminders**: "remind me" via the Notifications API (client-side scheduling is fine for v1).

---

## Station catalogue (`stations.json`)

Hand-curated, seeded from the Priyom station pages (English, German, Slavic, Other, Morse, Digital and Operators categories under `https://priyom.org/number-stations/…`). Seeding must be reviewed by a human and must keep attribution.

```json
{ "V13": { "name": "New Star Broadcasting", "priyomUrl": "...", "txSite": {"lat": 0, "lon": 0, "label": "..."},
           "typicalDurationMin": 20, "language": "Chinese", "notes": "" } }
```

Unknown stations get a default 10 min duration, and a probe showing the carrier gone ends "on now" early.

### Mode mapping (Priyom → UberSDR)

| Priyom | UberSDR |
|---|---|
| `AM` | `am` (offer `sam`) |
| `USB` / `USB/AM` | `usb` |
| `LSB` | `lsb` |
| `CW` | `cwu` |
| `MCW` | `am` |
| Digital (`RTTY`, `FSK`, `MFSK`, `PSK`, `OFDM`, …) | `usb`, with a UI note that it is a digital mode |

**VERIFY** whether UberSDR applies a CW offset, and that tuning to Priyom's listed frequency is correct for each mode. Mode strings seen in the feed so far: `USB/AM`, `RTTY`. Log any new ones.

---

## Ranking (v1 heuristic; keep the weights in config)

Hard filters:

- online
- the frequency is inside `tuning_range`
- `available_clients > 0`, unless we already hold a relay session on that receiver
- `load_status` is not overloaded
- the antenna is connected

Score components:

1. **Probe result**, which dominates when present: the signal's SNR at the frequency against neighbouring bins.
2. **Path plausibility**: great-circle distance from the transmitter site, judged by frequency and time of day. Below ~6 MHz at night favour roughly 300–2500 km; above ~10 MHz in daylight favour roughly 1500–6000 km. Penalise likely skip-zone distances. Use day/night at the path midpoint (compute solar elevation; don't rely on `is_daylight` alone).
3. **Receiver quality**: `snr_1_8_30_mhz`.
4. **Load**: prefer free slots.
5. **Later**: the PSKReporter path-open signal.

---

## Etiquette and limits (non-negotiable)

- **No UberSDR connection without a listener**, as set out in the idle connection policy.
- One shared upstream session per (receiver, frequency, mode), with a per-receiver cap, and respect for `max_session_time` and any rejection.
- Probes and spectrogram fetches are shared and cached; their volume must never scale with users.
- Clear `User-Agent` with a contact. No circumventing CORS, passwords or bans.
- No scraping Priyom beyond the 15-min poll.
- Visible attribution to Priyom and to the receiver operator wherever audio plays.

---

## Milestones

0. **(Tom)** Talk to Priyom about the feed. Confirm M0LTE is available as the dev target. (UberSDR author sign-off is already done.)
1. **Schedule MVP**: ScheduleService, DirectoryService, StationCatalog stub, Ranker (no probes), and the On now / Up next UI with **deep links** only. Shippable on its own.
2. **Relay + audio**: `/connection` and `/ws` upstream with Opus, a WebM muxer, the channels API, the in-page `<audio>` player and the audio spectrogram.
3. **RF waterfall + history strip**: spectrum WS relay and the cached spectrogram proxy.
4. **Probing**: Prober, "signal detected" badges, and probe-driven ranking and end-of-transmission detection.
5. **Extras**: reminders, an iCal feed per station, and the PSKReporter path heuristic.
6. **KiwiSDR provider.**

## Testing

- Unit tests:
  - the Priyom summary parser, with fixtures for multi-frequency, `Search`, remarks and unparseable entries
  - mode mapping
  - ranking
  - status derivation (upcoming / live / done)
  - the WebM muxer, checking the output plays in Chromium and Firefox
  - **teardown**, using a fake provider, with assertions that the upstream session closes within the grace period after:
    - a clean disconnect
    - an abrupt client drop
    - a pause
    - a receiver switch
    - an expiry with no listeners
  - that no upstream session is opened before Play
  - that the watchdog catches an artificially leaked refcount
- Integration tests against **M0LTE only**, gated behind an env var. CI must never touch public instances or Priyom.

## Open items for the implementer to investigate

- The exact `/ws` audio framing for `format=opus` and the current `version` value. Read `radio_client.py` and `app.js`.
- The `/ws/user-spectrum` handshake: whether it needs the same `user_session_id` (and an active audio session), how to set the zoomed span and centre, and the binary row format.
- Which endpoint gives the most practical probe: wideband FFT, rolling spectrogram `rowspectrum`, or a brief spectrum session. Measure the bin width at numbers-station frequencies.
- Whether `/api/spectrogram` covers out-of-ham-band frequencies (the "wideband" recorder suggests yes).
