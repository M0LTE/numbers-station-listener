# UberSDR client protocol: implementer's spec

What a .NET relay needs to talk to an UberSDR instance: session registration, the audio and spectrum WebSockets, the HTTP history and probe endpoints, and the per-IP limits a single-IP relay has to live within.

## 0. Provenance and conventions

- Source read: `madpsy/ka9q_ubersdr`, cloned at `/home/tf/work/ref/ka9q_ubersdr`, commit `ced24e1` (2026-10-06 21:50 +0100). The code is GPL-3.0. This document describes formats and constants; it contains no UberSDR code.
- Citations are `file:line` relative to the repository root, for example `websocket.go:451`.
- Live checks were made against Tom's own instance only, `https://reading-ubersdr.m0lte.uk/` (callsign M0LTE, reports `version` 0.1.66), on 2026-10-06 at about 21:59 UTC. Section 10 lists what was seen. Captured samples are in `tests/fixtures/ubersdr/` (layout in that directory's README).
- **CONFIRMED LIVE** marks behaviour seen on the wire. **VERIFY** marks anything not yet confirmed.
- All multi-byte binary fields are little-endian unless stated otherwise. "UUID" means a lowercase canonical UUID string.

## 1. Session model in one page

1. The client mints a UUID (`user_session_id`) and registers it with `POST /connection`. This stores the request's `User-Agent` against the UUID and binds the UUID to the client IP (`main.go:4091-4106`).
2. The client opens `WS /ws` (audio) with that UUID. Optionally it opens `WS /ws/user-spectrum` (spectrum and waterfall) with the same UUID.
3. A UUID can own at most one audio session and one spectrum session. Opening a second audio socket with the same UUID destroys the first (`session.go:698-711`), and the same goes for spectrum (`session.go:991-1036`).
4. Every limit counts **UUIDs**, not sockets: `max_sessions` (global unique users), `max_sessions_ip` (unique UUIDs per IP), `max_session_time` (measured from when the server first saw the UUID), and the per-IP daily budget (one clock per UUID).
5. When anything kicks a UUID (inactivity, max session time, spectrum-only timeout, admin kick), the UUID is blacklisted for **1 hour** (`session.go:494`, `session.go:3719`). After that, `/connection` answers 410 for it and both sockets refuse it. The only way back is a new UUID.

For the relay, one upstream **channel** (receiver, frequency, mode) = one UUID = one audio socket plus, when wanted, one spectrum socket on the same UUID.

## 2. `POST /connection`

Handler: `handleConnectionCheck`, `main.go:3894-4113`. Route: `main.go:2889`.

### Request

```
POST /connection
Content-Type: application/json
User-Agent: <must be non-empty, see below>

{"user_session_id": "<uuid>", "password": "<optional bypass password>"}
```

- `user_session_id` must match `^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$` (`websocket.go:24-33`). That means lowercase only and an RFC 4122 version and variant nibble. .NET `Guid.NewGuid().ToString()` ("D" format, lowercase, version 4) passes.
- `password` is the operator's bypass password. Never send it. If one is sent and is wrong, the server answers 403 straight away (`main.go:3919-3926`).

### Response body (always JSON, `ConnectionCheckResponse`, `main.go:3881-3891`)

| Field | Type | Meaning |
|---|---|---|
| `client_ip` | string | The IP the server attributes to us. |
| `allowed` | bool | Go/no-go. |
| `reason` | string | Present on refusal; human-readable. |
| `session_timeout` | int s | Inactivity timeout. **Quirk:** when the real inactivity timeout is 0 (disabled), this echoes `max_session_time` instead (`main.go:3959-3962`). CONFIRMED LIVE: M0LTE returns 3600 here with `max_session_time` 3600. So a non-zero value does not prove an inactivity timeout exists. |
| `max_session_time` | int s | Lifetime cap for this UUID, 0 = unlimited. |
| `bypassed` | bool | Whether we are treated as a bypassed (trusted) client. |
| `allowed_iq_modes` | string[] | Wide IQ modes available to us. Irrelevant for this project. |
| `daily_time_used_secs` | int64 | Seconds used by our IP in the rolling 24 h window (0 if no limit). |
| `daily_time_remaining_secs` | int64 | Seconds left, or -1 for unlimited. |

### Status codes, in the order the checks run

| Status | `reason` | Cause | Code |
|---|---|---|---|
| 405 | "Method not allowed, use POST" | Not POST (an `OPTIONS` preflight only gets 204 if the CORS middleware handles it first, section 9.2). | `main.go:3898-3905` |
| 400 | "Invalid request body" | Body is not JSON. | `main.go:3908-3916` |
| 403 | "Invalid bypass password" | Wrong password sent. | `main.go:3919-3926` |
| 403 | "This receiver requires a password to access" | Operator set `bypassed_users_only`. | `main.go:3933-3940` |
| 429 | "Rate limit exceeded. Please wait before trying again." | Per-IP `/connection` rate limit, section 2.1. | `main.go:3943-3950` |
| 403 | "Your IP address has been banned" | IP ban. | `main.go:4013-4021` |
| 403 | "Your browser or client has been banned" | User-Agent matches an operator ban regex. | `main.go:4024-4032` |
| 400 | "Invalid or missing user_session_id" | UUID fails the regex above. | `main.go:4034-4040` |
| 410 | "Your session has been terminated. Please refresh the page." | This UUID was kicked within the last hour. | `main.go:4043-4049` |
| 503 | "Maximum unique users reached (N of M)" | Global `max_sessions` full. | `main.go:4052-4063` |
| 503 | "Maximum unique users per IP reached (N)" | `max_sessions_ip` UUIDs already active from our IP. | `main.go:4066-4076` |
| 429 | "Daily time limit reached (N minutes per 24 hours)..." | `max_daily_time_per_ip` exhausted. | `main.go:4079-4088` |
| 200 | (none) | Allowed. | `main.go:4110-4112` |

Country and ASN bans are enforced earlier, by `banMiddleware` on every route (`main.go:318-342`, `main.go:3575`), and return 403 with a JSON or HTML body rather than this schema. Treat any 403 from any endpoint as "do not use this receiver".

Note: `allowed: true` reserves nothing. The same capacity checks run again when the WebSocket creates its session (`session.go:647-695`), and they can fail there.

### 2.1 Rate limit

- Default **10 requests per minute per IP**, token bucket with burst 10 and refill 10/60 per second (`ratelimit.go:508-541`; default `config.go:1357-1358`; operator key `server.sessions_per_minute`). Bypassed clients are exempt.
- The budget is per instance and per IP, so it is shared by every channel and every probe our relay starts on that receiver. One `/connection` per new UUID is the minimum. Re-POSTing an existing UUID costs a token and achieves nothing unless the registration has lapsed (below).

### 2.2 Does `User-Agent` matter?

Yes, in three ways:

1. **It is required.** The User-Agent mapping is only stored when the header is non-empty (`main.go:4091-4095`), and both WebSocket endpoints refuse a UUID with no stored User-Agent ("Invalid session. Please refresh the page and try again.": `websocket.go:617-622` for audio, `user_spectrum_websocket.go:230-234` for spectrum). A client with no User-Agent can register but can never connect.
2. Operators can ban by User-Agent regex. `/connection` checks it explicitly (`main.go:4024-4032`); every other route checks it in `banMiddleware` (`main.go:5392-5418`).
3. It is shown to the operator against the session (`session.go:3097-3107`). Send `numbers-station-listener/<version> (+<site url>; <contact>)`.

### 2.3 How long a registration stays valid before `/ws` must connect

The registration (User-Agent and IP binding) is dropped by `cleanupOrphanedUserAgents` once the UUID has had **no active session for 5 minutes** (`session.go:2615-2660`, constant at `session.go:2616`; the sweep runs every 2 s, `session.go:2380`). The 5 minutes count from the `/connection` call, or from the moment the UUID's last socket closed. Within that window a closed socket can be reopened under the same UUID without a new `/connection`. After it, both sockets answer "Invalid session" and a new `/connection` is needed.

`enforce_session_ip_match` (default false, `config.go:1233`) additionally requires the WebSocket to come from the IP that registered the UUID. A relay always uses one IP, so this is harmless.

## 3. `WS /ws` (audio)

Handler: `HandleWebSocket`, `websocket.go:409-833`. Message loop: `websocket.go:901-1697`. Streaming: `websocket.go:1924-2644`.

### 3.1 Query parameters

| Param | Required | Values | Default | Notes |
|---|---|---|---|---|
| `user_session_id` | yes | UUID registered via `/connection` | | `websocket.go:595-604`. |
| `frequency` | no | integer Hz within the receiver's tuning range | 14074000 | Parsed with `%d`. Out of range: JSON error then close (`websocket.go:540-555`). Range is `receiver.min_frequency`..`max_frequency`, defaulting to 10 kHz..span (`receiver_span.go:114-127`). M0LTE: 10000 to 30000000 (CONFIRMED LIVE in `/api/description` `tuning_range`). |
| `mode` | no | `usb lsb am sam fm nfm cwu cwl iq`; `iq48 iq96 iq192 iq384` only if bypassed or the operator made them public | `usb` | `websocket.go:557-593`. Invalid: JSON error then close. |
| `bandwidthLow`, `bandwidthHigh` | no | integer Hz, each within -12000..+12000, relative to `frequency` | per-mode default, 3.3 | Only used if **both** are given; otherwise the mode default applies (`websocket.go:640-686`, `websocket.go:782-800`). Out of range: error then close. Ignored for wide IQ modes. |
| `format` | **send it** | `opus` or `pcm-zstd` | **`pcm-zstd`** | `websocket.go:486-500`. Anything else: HTTP 400 before upgrade. The default is lossless PCM, so `format=opus` must be explicit. |
| `version` | **send it** | 1, 2, 3, 4 | 1 | `websocket.go:432-461`. Outside 1..`pcmMaxProtocolVersion` (4, `pcm_v4_header.go:141`): HTTP 400 before upgrade. Current clients send 4 (`static/v2/src/radio/audio-connection.js:50`, `static/minimal-radio.js:29`, `clients/python/radio_client.py:737`); the legacy v1 UI still sends 2 (`static/websocket-manager.js:136`). |
| `password` | no | bypass password | | Never send. |
| `min_snr`, `min_power` | no | float, -999..+999; -999 = off | -999 | Audio gate: below threshold the server substitutes silence (`websocket.go:688-713`). |
| `muted` | no | bool | false | Start muted (`websocket.go:715-727`). |
| `min_margin` | no | float dB | 0 | Reduced-depth IQ only (`websocket.go:463-485`). Ignore. |

Recommended URL for the relay:

```
wss://<host>/ws?frequency=<Hz>&mode=<mode>&bandwidthLow=<lo>&bandwidthHigh=<hi>&user_session_id=<uuid>&format=opus&version=4
```

### 3.2 Modes, default passbands and sample rates

`defaultBandwidthForMode`, `websocket.go:836-870`; sample rates `config.go:2054-2078`.

| Mode | Default low..high (Hz, relative to `frequency`) | Audio sample rate | Notes |
|---|---|---|---|
| `usb` | +50..+2700 | 12000 | |
| `lsb` | -2700..-50 | 12000 | |
| `am`, `sam` | -5000..+5000 | 24000 | |
| `cwu`, `cwl` | -200..+200 | 12000 | Symmetric about the carrier; tone offset, see section 5. |
| `fm` | -8000..+8000 | 24000 | |
| `nfm` | -5000..+5000 | 24000 | |
| `iq` | -6000..+6000 | 12000, 2 channels | Always forces lossless PCM framing. Not for us. |

### 3.3 Pre-upgrade and post-upgrade failures

Some refusals are plain HTTP errors before the upgrade; most happen **after** the upgrade, as a JSON `error` text frame followed by the server dropping the TCP connection (no close frame).

Before upgrade (HTTP status, no WebSocket):

| Status | Cause | Code |
|---|---|---|
| 401 | IP banned | `websocket.go:419-423` |
| 400 | Unsupported `version` | `websocket.go:451-455` |
| 400 | Invalid `format` | `websocket.go:496-500` |
| 429 | Per-IP WebSocket connection rate (section 9.1) | `websocket.go:503-507` |
| 403 | Country, ASN or User-Agent ban (middleware) | `main.go:318-342` |

After upgrade (`{"type":"error","error":"..."}` then close): invalid frequency, invalid mode, wide IQ without bypass, invalid UUID, kicked UUID ("Your session has been terminated..."), unregistered UUID ("Invalid session..."), IP mismatch, bandwidth out of range, invalid gate or mute values, and every session-creation failure (`websocket.go:733-747`): global users full, per-IP UUIDs full, daily limit, radiod channel cap (2000, `session.go:24`), session-creation rate ("too many session attempts...", section 9.1).

A relay must therefore treat "socket opened, then one text frame with `type: error`, then close" as a refusal, not as a working session.

### 3.4 Binary audio frames for `format=opus`

One WebSocket binary message = one header + one Opus packet. The encoder is per connection, mono, `OPUS_APPLICATION_VOIP`, bitrate `audio.opus.bitrate` (default 24000) and complexity 5 (`opus_support.go:34-59`, `websocket.go:1960-1977`). radiod delivers 20 ms blocks and the server drops any block whose length disagrees with its rate (`websocket.go:2414-2427`), so **every Opus packet is one 20 ms frame**.

CONFIRMED LIVE (usb, 12 kHz): every packet's TOC byte has config 9 (SILK, mediumband, 20 ms), mono, code 0 (one frame per packet). Payloads were 97 to 129 bytes, average about 110, so the operator's bitrate is above the 24 kbit/s default.

Frames arrive at 50 per second while the session runs. When the squelch or audio gate is closed, or the session is muted, the server still sends 20 ms frames of encoded silence, so the timeline has no gaps (`websocket.go:2398-2412`). A separate 10 Hz ticker can also inject encoded-silence frames when no audio has been sent for 200 ms, but only for version >= 2 (`websocket.go:2074-2196`). Those use the server's wall clock as timestamp, not GPS time.

#### Version 1 (13-byte header) `websocket.go:2474-2485`

| Offset | Size | Type | Field |
|---|---|---|---|
| 0 | 8 | u64 | Timestamp, ns since the Unix epoch, GPS-disciplined capture time of the first sample |
| 8 | 4 | u32 | Sample rate, Hz |
| 12 | 1 | u8 | Channels (1) |
| 13 | n | bytes | Opus packet |

#### Versions 2 and 3 (21-byte header) `websocket.go:2458-2473`

| Offset | Size | Type | Field |
|---|---|---|---|
| 0 | 8 | u64 | Timestamp, ns |
| 8 | 4 | u32 | Sample rate, Hz |
| 12 | 1 | u8 | Channels |
| 13 | 4 | f32 | Baseband power, dBFS (gain-adjusted) |
| 17 | 4 | f32 | v2: noise density N0, dBFS/Hz. v3: noise power in the demodulator passband, dBFS |
| 21 | n | bytes | Opus packet |

With v3, `basebandPower - noise` is an SNR in dB; with v2 it is S/N0 in dB-Hz (`websocket.go:1700-1725`). Either figure is -999 when radiod has no reading. CONFIRMED LIVE (v3, 7910 kHz usb): power about -86 dBFS, noise -93 to -97 dBFS, so SNR 7 to 11 dB on band noise.

#### Version 4 (variable header, about 6 bytes) `pcm_v4_header.go:279-375`

Stateful: fields are sent only when they change, so frames must be parsed in order, one decoder state per socket.

```
[flags u8]
[timestamp]      if flags.bit1: u64 absolute ns; else signed zigzag varint delta ns from previous frame
[sampleRate]     if flags.bit1: unsigned LEB128 varint (Go binary.Uvarint), Hz
[channels u8]    if flags.bit1
[power i16][noise i16]   if flags.bit0: centi-dB (value / 100 = dB); -32768 = no reading
[Opus packet]    the rest of the message
```

- `flags` bit 0 = quality present, bit 1 = metadata present (also means the timestamp is absolute). Bits 2-7 are always 0, so an Opus v4 frame's first byte is 0x00..0x03 (`pcm_v4_header.go:157-162`, `pcm_v4_header.go:279-301`).
- The signed varint is Go's `binary.PutVarint`: zigzag, then LEB128 (`pcm_v4_header.go:340-349`; Python reference decoder `clients/python/pcm_v4.py:146-168`, `pcm_v4.py:291-338`).
- Metadata (absolute timestamp, rate, channels) is re-sent on the first frame, on any rate or channel change, on a backwards or more-than-5 s timestamp jump, and every 5 s regardless (`pcm_v4_header.go:323-337`, `pcm_v4_header.go:173`).
- Quality is sent on every metadata frame and whenever the centi-dB pair changes. The noise figure has version 3 semantics (passband power). Converting: `dB = i16 / 100`; -32768 means -999 / no reading (`pcm_v4_header.go:177-222`).

CONFIRMED LIVE: 251 frames in 5 s; 2 had flags 0x03, 52 had 0x01, 197 had 0x00; average header 5.9 bytes. **The first frame carried an absolute timestamp of 0**, and the second frame re-synchronised with the real time. After that, deltas were 20,000,000 ns, with a handful up to 20,012,179 ns. Do not use the first frame's timestamp, and do not drive playback timing from timestamps at all (section 8).

#### Telling frame kinds apart

The server can send non-Opus binary frames on a socket that asked for Opus: if the server was built without Opus support it silently falls back to `pcm-zstd` (`websocket.go:1970-1977`, `websocket.go:2371-2376`), and IQ modes always use PCM (`websocket.go:2363-2369`). With `version=4` the test is cheap and exact (`pcm_v4_header.go:279-301`):

- first byte <= 0x03: Opus v4 frame;
- first four bytes `50 43 4D 34` ("PCM4"): lossless PCM v4 frame (not for us; log and drop the session);
- anything else: unknown, drop.

With versions 1-3 there is no reliable discriminator, which is one more reason to ask for version 4. If version 4 is refused with HTTP 400 (servers older than 0.1.63, per `clients/python/radio_client.py:1030-1033`), fall back to 3 and use the 21-byte layout.

### 3.5 Text (JSON) messages

All server-to-client control messages on the audio socket are plain text frames (`websocket.go:220-239`). The message shape is `ServerMessage`, `websocket.go:385-406`.

Server to client:

| `type` | When | Fields |
|---|---|---|
| `status` | Reply to `get_status`, and after a `tune` that changed something. **Not** sent unprompted at connect for binary formats (`websocket.go:808`). | `sessionId`, `frequency`, `mode`, `sampleRate`, `channels`, `info` {id, channel, ssrc, frequency, mode, bandwidth, sample_rate, channels, created_at, last_active}, `agc` {agcHangTime, agcRecoveryRate, agcThreshold}, `clockId` (`websocket.go:2714-2727`, `session.go:3407-3423`) |
| `pong` | Reply to `ping` | `serverTimeNs` (server Unix ns) (`websocket.go:1109-1114`) |
| `error` | Any rejected command; fatal errors at connect | `error` (text), `status` (e.g. 429 for command rate limit) (`websocket.go:2730-2743`) |
| `squelch_updated`, `agc_state`, `agc_updated`, `dsp_status`, `mute_updated`, ... | Replies to the matching commands | `info` |

Client to server (text frames, JSON; `ClientMessage`, `websocket.go:312-342`):

| Message | Effect |
|---|---|
| `{"type":"tune","frequency":<Hz>,"mode":"<m>","bandwidthLow":<lo>,"bandwidthHigh":<hi>}` | Retune in place without reconnecting; every field optional. Validated like the URL. A mode change reloads radiod's preset and re-applies that mode's default passband unless a passband is in the same message. Replies with `status` if anything changed (`websocket.go:937-1107`). |
| `{"type":"ping"}` | Answers `pong`. **Touches the session's inactivity clock.** Not counted against the command rate limit. |
| `{"type":"get_status"}` | Answers `status`. |
| `{"type":"set_mute","muted":true}` | Mute or unmute (`websocket.go:1662-1692`). |
| `{"type":"set_squelch","squelchOpen":<dB>,"squelchClose":<dB>}` | -999 = always open (`websocket.go:1127-1176`). |
| `set_agc`, `get_agc`, `set_audio_gate`, `set_dsp`, `set_dsp_params`, `get_dsp_filters`, `set_min_margin` | Not needed by this project. |

Rules:

- Every message except `ping` counts against a per-UUID command limit of at least 50 per second (`websocket.go:925-934`, `config.go:1333-1335`); over the limit the command is refused with a 429 `error` and the socket stays open.
- Unknown `type` values are logged and ignored (`websocket.go:1694-1695`).
- **A frame that is not valid JSON, or a binary frame, ends the session**: the read loop breaks on any decode error (`websocket.go:904-912`). Never send anything but JSON text.

### 3.6 Keepalives and session end

- **WebSocket-level liveness.** The server sets no read deadline and sends no pings of its own. Gorilla answers protocol-level ping frames with pongs automatically while the read loop runs, and **protocol pings do not touch the session's activity clock** (only JSON messages do: `websocket.go:922`, `session.go:2078-2084`). The relay can therefore use WebSocket ping frames for its own dead-peer detection without affecting the operator's idle accounting.
- **Inactivity timeout (`server.session_timeout`).** Default 0 = disabled (`config.go:1329`; the example config ships 0, `config/config.yaml.example:250`). When it is non-zero, a UUID whose sockets have received no JSON message for that long is kicked and blacklisted for an hour (`session.go:2673-2746`). The official UI deliberately sends `{"type":"ping"}` only when the person is actually doing something, at most every 10 s, so an idle tab does lose its slot (`static/v2/src/radio/audio-connection.js:304-308`, `static/v2/src/radio/idle.js`). Relay policy: send `{"type":"ping"}` on the audio socket only while at least one browser is listening, at an interval well inside `session_timeout`, for example every 30 s. Because of the `/connection` quirk in section 2, assume `session_timeout` is real.
- **`max_session_time`** (example config 3600 s). Measured from the first time the server saw the UUID, not from the socket's open, and not reset by reconnecting (`session.go:611-616`, `session.go:2476-2534`). When it expires the UUID is kicked: every socket on it is closed and it is blacklisted for 1 hour.
- **Spectrum-only timeout** (section 4.6) also kicks the whole UUID.
- **How a kick looks on the wire.** `DestroySession` closes the TCP connection directly (`session.go:2259-2266`): no error frame and no close frame, so the client sees an abnormal closure (1006). Expect that on expiry, kick, ban, or server restart.
- **Normal close.** On a client close frame, the read loop ends, the server destroys the session and its radiod channel, and Gorilla echoes the close frame. CONFIRMED LIVE: clean close code 1000 on all three sockets, within 0.1 s.

## 4. `WS /ws/user-spectrum` (spectrum and waterfall)

Handler: `HandleSpectrumWebSocket`, `user_spectrum_websocket.go:130-344`. Message loop: `user_spectrum_websocket.go:383-655`. Encoders: v1 float `:723-882`, v1 binary8 `:884-1047`, v2 `user_spectrum_v2.go`.

### 4.1 Handshake

```
wss://<host>/ws/user-spectrum?user_session_id=<uuid>&mode=binary8&version=2&frequency=<centre Hz>&bin_bandwidth=<Hz per bin>
```

| Param | Notes |
|---|---|
| `user_session_id` | Required. Must be registered via `/connection` (same User-Agent check as audio, `user_spectrum_websocket.go:230-234`) and not kicked (`:222-228`). It does **not** need an active audio session to connect, but see the spectrum-only timeout in 4.6. Use the channel's audio UUID: audio and spectrum on one UUID count as one user. |
| `mode` | `binary8` for 8-bit frames. Absent or anything else = version 1 float32 frames (`:154-155`). |
| `version` | 1 or 2; default 1. Version 2 applies only together with `mode=binary8` (`:167-181`). Above 2: HTTP 400 before upgrade. The current UI sends `mode=binary8&version=2` (`static/v2/src/radio/spectrum-connection.js:232-236`). |
| `frequency` | Optional initial centre, float Hz (`:188-192`). |
| `bin_bandwidth` | Optional initial Hz per bin, float (`:193-197`). If you give `frequency` without this, you get the full-span bin width (about 29.3 kHz) centred there. Always send both. |
| `password` | Never send. |

Pre-upgrade refusals are HTTP errors: 401 banned IP, 400 bad version, 429 connection rate, 400 bad UUID, 403 kicked UUID, 400 unregistered UUID, 403 IP mismatch (`:139-250`). Session-creation failures come after the upgrade as a gzip `error` message followed by close (`:286-299`).

### 4.2 View geometry: centre, span, bins

- **Bin count is fixed by the operator** (`spectrum.bin_count`): 512, 1024 (default) or 2048 (`config/config.yaml.example:777-796`, `config.go:1505-1527`). The client cannot raise it. The server only lowers it, towards 256, when asked for less than 0.5 Hz per bin (`user_spectrum_websocket.go:567-577`).
- **Span = binCount x binBandwidth.** The `config` message reports both, plus `totalBandwidth`.
- **Zoom and pan snap the bin width to a ladder** (`user_spectrum_websocket.go:524-559`): 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 300, 500, 1000, 2000, 5000 Hz per bin; anything above 7500 passes through unchanged (the full-span default). The thresholds are <0.75 to 0.5, <1.5 to 1, <3 to 2, <7 to 5, <15 to 10, <35 to 20, <75 to 50, <150 to 100, <250 to 200, <400 to 300, <750 to 500, <1500 to 1000, <3500 to 2000, <7500 to 5000.
- **Initial view parameters at connect are applied without that snapping** (`user_spectrum_websocket.go:310-330` calls `UpdateSpectrumSession` with the raw value). radiod searches for a workable FFT size rather than rejecting, so off-ladder values probably work. VERIFY before relying on it; to be safe, send ladder values only.
- Centre must lie within the receiver's tuning range, otherwise the zoom is rejected with an error (`:467-486`). The UI also clamps so the edges stay inside 0..max (`static/v2/src/radio/spectrum-connection.js:393-412`).

**Achievable views around a 5 to 20 MHz numbers-station signal at a 10 to 20 kHz span** (the bin width does not depend on the RF frequency):

| Operator bin_count | ~10 kHz span | ~20 kHz span |
|---|---|---|
| 512 | 20 Hz/bin = 10.24 kHz | 50 Hz/bin = 25.6 kHz (nearest) |
| 1024 (default; M0LTE, CONFIRMED LIVE) | 10 Hz/bin = 10.24 kHz | 20 Hz/bin = 20.48 kHz |
| 2048 | 5 Hz/bin = 10.24 kHz | 10 Hz/bin = 20.48 kHz |

Pick `bin_bandwidth = desiredSpan / defaultBinCount`, snapped to the ladder, using `defaultBinCount` from the first `config` message (or `/api/description`). Each bin takes about 1/binBW seconds to integrate (10 Hz/bin about 0.1 s), which is why the UI stops zooming at 2 Hz/bin (`static/v2/src/radio/spectrum-connection.js:46-70`).

### 4.3 Server-to-client JSON (gzip, in binary frames)

**Every JSON message on this socket is gzip-compressed and sent as a binary frame** (`user_spectrum_websocket.go:1073`, `:1094`, `websocket.go:241-271`). Sort binary frames by their first bytes: `53 50 45 43` ("SPEC") is spectrum data, `1F 8B` is gzip JSON.

| `type` | When | Fields |
|---|---|---|
| `config` | At connect, and after every `zoom`, `pan`, `reset`, `set_rate`, `get_status` | `centerFreq`, `binCount`, `binBandwidth`, `totalBandwidth`, `sessionId`, `defaultBinCount`, `defaultBinBandwidth` (`:1049-1074`) |
| `error` | Rejected command, or session-creation failure (then close) | `error`, `status` |
| `pong` | Reply to `ping` | |

CONFIRMED LIVE: `{"binBandwidth":10,"binCount":1024,"centerFreq":7910000,"defaultBinBandwidth":29296.875,"defaultBinCount":1024,"sessionId":"...","totalBandwidth":10240,"type":"config"}`.

### 4.4 Client-to-server commands (JSON text frames)

| Message | Effect |
|---|---|
| `{"type":"zoom","frequency":<Hz>,"binBandwidth":<Hz>}` | Set centre and/or bin width; either field optional. `pan` is identical (`:456-615`). Frequency may be int or float. Reply: `config`. |
| `{"type":"reset"}` | Back to the shared full-span default view (`:420-454`). |
| `{"type":"set_rate","divisor":<1..8>}` | Poll at 1/N of the normal rate, private (zoomed) channels only (`:617-641`). |
| `{"type":"ping"}` | Reply `pong`; touches the session's activity clock. |
| `{"type":"get_status"}` | Reply `config`. |

Same rules as audio: non-ping commands count against the >= 50/s per-UUID limit, and **any frame that is not valid JSON closes the connection** (`:386-397`).

### 4.5 Binary spectrum frames

Rate: one frame per poll, `spectrum.poll_period_ms` default 100 ms = 10 frames/s (`config/config.yaml.example:798-801`). CONFIRMED LIVE: 10 per second. Frames are dropped, not queued, if the client falls 30 frames behind (`websocket.go:163-211`; buffer of 30 at `websocket.go:168`).

**Bin order: raw FFT order.** Bin 0 is DC (the centre frequency), rising to +Nyquist, then -Nyquist up to just below DC. To get ascending frequency, rotate left by `floor(n/2)`: `ordered[i] = raw[(i + floor(n/2)) mod n]` (`static/v2/src/radio/spectrum-connection.js:14-17`, `:627-657`, `:744-756`). Deltas index raw order, so keep the accumulator in raw order and rotate on output. After rotation, ordered bin k is centred at `centre + (k - floor(n/2)) x binBW` by FFT convention. The UI instead draws bin k as covering `[centre - span/2 + k x binBW, + binBW)` (`static/v2/src/lib/ifSpectrum.js:407-414`), which is half a bin different and irrelevant at 10 Hz/bin. VERIFY the ordering and the half-bin against a known carrier; the live capture had no clear carrier to check it on.

Header timestamps on spectrum frames are the server's wall clock at send time, in **nanoseconds**, despite comments saying milliseconds (`user_spectrum_websocket.go:767`, `:951`; `user_spectrum_v2.go:410`). The frequency field is the session's current centre.

#### Version 1 (`mode=binary8` omitted or `version=1`): 22-byte header

| Offset | Size | Type | Field |
|---|---|---|---|
| 0 | 4 | bytes | `53 50 45 43` "SPEC" |
| 4 | 1 | u8 | Version = 1 |
| 5 | 1 | u8 | Flags: 0x01 full float32, 0x02 delta float32, 0x03 full uint8, 0x04 delta uint8 |
| 6 | 8 | u64 | Timestamp, ns |
| 14 | 8 | u64 | Centre frequency, Hz |
| 22 | | | Body |

- 0x01 full float32: binCount x f32 dBFS (`user_spectrum_websocket.go:771-810`).
- 0x02 delta float32: u16 changeCount, then changeCount x {u16 index, f32 value} (`:811-865`).
- 0x03 full uint8: binCount x u8, `dB = code - 256` (0 = -256 dB, 255 = -1 dB; values truncated, and >= 0 dBFS wraps to 0, a known v1 bug) (`:895-906`, `:956-993`).
- 0x04 delta uint8: u16 changeCount, then changeCount x {u16 index, u8 value} (`:994-1037`).
- Delta rule: a bin is sent when it moved more than `spectrum.delta_threshold_db` (default 3.0, range 1..10, `config.go:1615-1626`); when more than 80% of bins changed a full frame is sent instead. No keyframes and no sequence numbers, so a dropped delta corrupts bins until the next full frame. Avoid version 1.

#### Version 2 (`mode=binary8&version=2`): 24-byte header (`user_spectrum_v2.go:47-66`, `:362-386`)

| Offset | Size | Type | Field |
|---|---|---|---|
| 0 | 4 | bytes | "SPEC" |
| 4 | 1 | u8 | Version = 2 |
| 5 | 1 | u8 | Flags: 0x05 full, 0x06 delta |
| 6 | 2 | u16 | Sequence, +1 per frame, wraps at 65536 |
| 8 | 8 | u64 | Timestamp, ns |
| 16 | 8 | u64 | Centre frequency, Hz |
| 24 | | | Body |

- **0x05 full:** `i16 refCentiDB`, `u8 stepCentiDB`, then binCount x u8 codes. `dB = (refCentiDB + code x stepCentiDB) / 100`. The scale is chosen from the data with 6 dB margin each side; step 0.25 to 2.55 dB (`user_spectrum_v2.go:122-176`). binCount = body length - 3.
- **0x06 delta:** a bitmask of `ceil(binCount/8)` bytes, then one u8 code per set bit, in ascending bin order. Bit `i & 7` of mask byte `i >> 3` (LSB first) is raw bin i (`user_spectrum_v2.go:326`). Valid only if body length = mask length + popcount(mask). Codes use the scale from the most recent full frame. Unsent bins keep their previous code.
- A full frame is sent on the first frame, when the bin count changes, after a dropped frame, when the data outgrows the scale, when a delta would be no smaller than a full frame, and at least every 50 frames (5 s at 10 fps) (`user_spectrum_v2.go:95`, `:269-347`).
- A sequence gap means the server dropped a frame for a slow reader; the next frame is then a full one.
- **Gotcha, CONFIRMED LIVE: a `zoom` that keeps the bin count does not force a full frame.** After zooming 10 to 20 Hz/bin, the server sent a `config` and then went straight on with deltas against codes from the old geometry. Bins that did not move by more than 3 dB keep stale values from the old view for up to 5 s, until the next keyframe. Set the view at connect time with `frequency` and `bin_bandwidth`. If a channel needs a different view, reconnect the spectrum socket, or blank the display until the next 0x05 frame.

CONFIRMED LIVE (10 Hz/bin at 7910 kHz): first full frame ref -145.33 dB, step 0.25 dB, values -139 to -107 dBFS; deltas changed 116 to 525 of 1024 bins and were 268 to 677 bytes long.

### 4.6 Spectrum-only timeout

`server.spectrum_only_timeout` (example config 60 s, `config/config.yaml.example:266-273`; code default 0 = disabled when the key is absent, `config.go:375`). A non-bypassed UUID that has a spectrum session but no audio session for longer than this is kicked and blacklisted for 1 hour (`session.go:2391-2465`). The clock starts when a UUID first connects spectrum-only (`session.go:1144-1151`), or when its audio socket closes (`session.go:2205-2211`).

Consequences for the relay:

- Never hold an upstream spectrum socket without the audio socket on the same UUID. If a browser wants the waterfall only, either open audio too (muted is fine, `muted=1`) or do not offer waterfall-only.
- If the upstream audio socket drops, reconnect it or close the spectrum socket within 60 s.
- A spectrum-only **probe** is fine if it finishes well inside 60 s. Brief's 10 s cap is safe.

### 4.7 Cost of a spectrum socket

It uses one unique-user slot (shared with audio on the same UUID), one WebSocket connection token, one session-creation token, and, once zoomed, a private radiod channel. The radiod channel cap is 2000 (`session.go:24`, `session.go:575-586`). `spectrum.max_sessions_per_user` exists in config (`config.go:499`, default 2) but no enforcement was found in the code.

## 5. Mode handling and tuning recipes

### 5.1 What `frequency` means

`frequency` is always the channel's tuning frequency: the carrier for AM and SAM, the suppressed-carrier (dial) frequency for USB and LSB, and the carrier for CW. The passband edges in `bandwidthLow`/`bandwidthHigh` are offsets from it (section 3.2). The server never adds an offset to `frequency`; it is sent to radiod as given (`websocket.go:540-555`, `session.go` `UpdateSessionChannel`).

### 5.2 CW offset

- There is **no offset on the RF side**: `cwu`/`cwl` use a filter of -200..+200 Hz centred **on** `frequency` ("200 Hz either side of carrier", `config/bookmarks.yaml.example:125-130`; "CW is symmetric about the carrier despite the name", `static/v2/BRIDGE_API.md:296-297`).
- The offset is on the **audio** side: radiod shifts the demodulated output so a carrier exactly on `frequency` comes out as a **500 Hz tone** (`static/v2/src/lib/audioBand.js:11-17`, `CW_OFFSET = 500`; v1 equivalent `static/app.js:7971-7980`). This comes from radiod's `cwu`/`cwl` presets (ka9q-radio `presets.conf`), which are not in this repository. VERIFY by ear or audio FFT that an on-frequency carrier gives 500 Hz, and whether `cwl` gives the same tone.
- So for "17437kHz CW" send `frequency=17437000&mode=cwu`. No adjustment.
- **Practical gotcha:** Priyom lists frequencies to the whole kHz, and real transmitters can be a few hundred Hz off. A +/-200 Hz filter can miss them. Widen it, for example `bandwidthLow=-450&bandwidthHigh=450`. Do not go past about -500 on the low edge: with a +500 Hz audio shift, anything more than 500 Hz below the dial folds around 0 Hz audio (inference from the shift; VERIFY). If wider coverage is needed, use `usb` with the dial 1 kHz low instead (below).

### 5.3 Recipes for the Priyom examples

| Priyom listing | Send | Why |
|---|---|---|
| `15388kHz USB/AM` (for example V13) | `frequency=15388000&mode=usb` (default passband +50..+2700) | Priyom's `USB/AM` means a full or reduced carrier with at least the upper sideband, receivable in USB or AM. The listing is the carrier, which is exactly the USB dial. `usb` is narrower, so it has less noise than `am` (+/-5 kHz). Offer `am` or `sam` as an alternative (`sam` locks to the carrier and tolerates small offsets; `am` is plain envelope detection). |
| `8175kHz RTTY` (F06) | `frequency=8173500&mode=usb` (that is, listed - 1500 Hz), passband default +50..+2700 | FSK is not audible in any mode tuned exactly on its centre with a USB filter: the lower tone would fall below the +50 Hz edge. Putting the dial 1.5 kHz below the listed frequency places the tones around 1500 Hz audio (for example 1400/1600 Hz for a 200 Hz shift, 1075/1925 Hz for 850 Hz), well inside the passband, and the waterfall shows both tones. **VERIFY** Priyom's convention: utility listings usually give the centre (assigned) frequency, but some give the mark. If it is the mark, the tones land at 1500 Hz and 1500 +/- shift, which is still inside the passband for shifts up to 1 kHz, so the recipe works either way. Keep the brief's UI note that this is a digital mode. |
| `17437kHz CW` (M12) | `frequency=17437000&mode=cwu&bandwidthLow=-450&bandwidthHigh=450` | See 5.2. Alternative with more tolerance: `frequency=17436000&mode=usb&bandwidthLow=300&bandwidthHigh=2000`, where an on-frequency carrier is a 1 kHz tone and +/-700 Hz errors are still audible. |

For `MCW` (tone-modulated CW, an AM signal) use `am` at the listed frequency, as the brief says. For `LSB` use `lsb` at the listed frequency.

### 5.4 Blocked ranges

An operator can mark frequency ranges as blocked: a band in `bands.yaml` with group `blocked`. Listeners in that range hear a looping recorded announcement instead of the receiver (`audio_blocked.go:1-45`). Bands are public at `GET /api/bands` (`main.go:2906`, `handleBands` `main.go:4755`; band objects `{label, start, end, group, mode, button_name}`, `config.go:339-346`). Before choosing a receiver, fetch `/api/bands` (cache it), and skip receivers where a band with group equal to `blocked` (case-insensitive, trimmed) covers the frequency. VERIFY the exact top-level JSON shape of `/api/bands`.

## 6. Recorded spectrogram (`/api/spectrogram` family)

Code: `spectrogram_recorder.go`; routes `main.go:3060-3091`.

### 6.1 What is recorded

- The wideband recorder covers **0 Hz to the receiver span**: 0 to 30 MHz on a 64.8 Msps receiver. It is not limited to amateur bands (`spectrogram_recorder.go:118-134`). CONFIRMED LIVE: `start_freq_hz` 0, `end_freq_hz` 30000000.
- Bins: the smallest power of two giving at most 7324.21875 Hz per bin (`receiver_span.go:57`, `:238-248`). That is **4096 bins of 7324.22 Hz** on 30 MHz (CONFIRMED LIVE).
- **One row per minute** (up to 1440 per UTC day), on the minute (`spectrogram_recorder.go:264-343`). Each row is the noise-floor monitor's **10-second average** at that moment (`noise_floor.go:1483-1484`), so it samples only the last 10 s of each minute. A short transmission can fall between rows.
- One file per UTC day. Row 0 is 00:00 UTC, at the top of the PNG. A PNG is 1 pixel per bin wide and 1 pixel per row high (`spectrogram_recorder.go:1386-1428`).
- Per-band recorders also exist (`?band=80m` and so on, `NewBandSpectrogramRecorder` `:136-147`). These cover amateur bands only, so they are of no use here. `band=wideband-hf` is the wideband recorder cropped to 1.8 MHz and above.

### 6.2 Endpoints

| Endpoint | Params | Response | Rate limit (per IP, token bucket) |
|---|---|---|---|
| `GET /api/spectrogram` | `date=YYYY-MM-DD` (default today), `rolling=1` (last 24 h, from memory), `palette=jet|viridis|plasma`, `db_min`, `db_max`, `freq_min`, `freq_max` (Hz), `band` | PNG. **`freq_min`/`freq_max` crop the image** to those bins (`spectrogram_recorder.go:2191`, `:2214`, `:2238`), even though an older comment says they only affect auto-range. Without `db_*` the range is auto (P5..P95). | 1/s on key `spectrogram`; 1/s on key `spectrogram-palette` when `palette`/`db_*` are given (`spectrogram_recorder.go:2147-2157`, `ratelimit.go:274-276`). |
| `GET /api/spectrogram/latest` | | 302 to the most recent complete day's PNG | 1/s (`spectrogram-latest`) |
| `GET /api/spectrogram/meta` | `date`, `rolling=1`, `freq_min`, `freq_max`, `band` | JSON: `date, start_freq_hz, end_freq_hz, bin_width_hz, bin_count, row_count, max_rows, row_interval_seconds (60), db_min, db_max, palette, image_url, list_url, complete, rows[{row, utc_time, unix, noise_floor, peak_db}]` (`:2567-2879`). About 119 KB for 926 rows. | 5/s (`spectrogram-meta`) |
| `GET /api/spectrogram/meta/latest` | | 302 to meta for the most recent complete day | 1/s |
| `GET /api/spectrogram/timeslice` | `freq_hz`, `bandwidth_hz` (both required), `date` or `rolling=1` | JSON `{freq_hz, bandwidth_hz, source_band, bin_width_hz, bins_averaged, start_freq_hz, end_freq_hz, date, rows[{unix, utc_time, db, noise_floor?}]}`: the mean dB of the bins covering freq +/- bw/2, one entry per minute. Rows with no data are **omitted**, not null (`:3465-3702`). | 10/s (`spectrogram-timeslice`) |
| `GET /api/spectrogram/rowspectrum` | `row` (required), `date` or `rolling=1` | JSON with all bins of one row, `bins` rounded to 0.1 dB, null for no data (`:3095-3290`) | 2/s |
| `GET /api/spectrogram/allrows` | `date`, `rolling=1`, `band` | JSON of every row and every bin (large) | 0.5/s, burst 2 |
| `GET /api/spectrogram/thumb` | `date` or `rolling=1` (+ palette, db, freq crop) | 300x168 PNG | none |
| `GET /api/spectrogram/list`, `/thumbnails` | | JSON lists | none |

The brief's "1 PNG per 10 s" is out of date: the limiter is 1 per second per key (`ratelimit.go:274-276`). The `main.go:3060` comment still says 10 s. Cache anyway: today's PNG and meta are `max-age=60` and only change once a minute.

### 6.3 "The last 30 minutes around a frequency"

There is no time-window parameter. Two options:

1. **Recommended: `timeslice`.** `GET /api/spectrogram/timeslice?freq_hz=<f>&bandwidth_hz=3000&rolling=1` gives one dB value per minute for the last 24 h; take the last 30 entries. Fetch a second slice 20 to 30 kHz away as the local reference. The `noise_floor` field is the P5 of the whole 0-30 MHz row, not local. It is small (5 KB live), allowed at 10/s, and lets us draw our own strip.
2. `GET /api/spectrogram?rolling=1&freq_min=<f-20000>&freq_max=<f+20000>&db_min=...&db_max=...` and keep the bottom 30 rows. At 7.3 kHz per bin, +/-20 kHz is only about 5 pixels wide, so this is a picture of almost nothing. Each distinct crop also takes one of the server's three cached rolling renders (`spectrogram_recorder.go:61`), so many crops make the operator's server re-render.

**Crop narrower than one bin falls back to the full width.** `binSliceForFreqRange` returns the whole 0-30 MHz range when the rounded start and end bins coincide (`spectrogram_recorder.go:1174-1201`), so a 12 kHz crop can come back as a 4096-pixel image. Ask for at least several bins; the provider uses 6 bins (about 44 kHz) as its minimum and refuses any image wider than 256 pixels.

Resolution is the limiting factor either way. A 3 kHz USB signal in a 7.3 kHz bin reads about 4 dB lower than it would in its own bandwidth, so weak SSB or CW will often not show. Strong AM broadcasters and the stronger numbers stations will.

CONFIRMED LIVE caveat: at capture time the M0LTE timeslice had only 75 rows, ending at 21:03 UTC, because the recorder had stopped getting FFT data (and the noise-floor endpoints returned 204, section 7). Rows without data are skipped and meta rows carry `noise_floor: 0`, so "no rows" or "all zeros" must be read as "no data", not as "no signal".

## 7. Noise-floor FFT and other cheap probes

### 7.1 `GET /api/noisefloor/fft?band=<name>`

`main.go:5872-5923`. `band` is required and must be one of the operator's noise-floor bands, which are amateur bands (160m ... 6m, `config/config.yaml.example:1514-1630`; list at `/api/noisefloor/config`). Response `BandFFT` (`noise_floor.go:319-327`): `{timestamp, band, start_freq, end_freq, bin_width, data: [dB...], markers: [...]}`, max-hold over one background poll period. Rate 1 per 2 s per band key per IP (`ratelimit.go:290-292`). 204 when no data. **No use for numbers stations**, apart from the few inside amateur bands.

### 7.2 `GET /api/noisefloor/fft/wideband`

`main.go:5926-5971`. Same `BandFFT` JSON for 0 to span: 4096 bins of 7324.22 Hz, a **10-second average**, cached for one background poll period (`noise_floor.go:1461-1500`). Rate 1 per 2 s per IP (key `wideband`). Gzip if requested; about 4096 floats of JSON.

Useful as a **current** wide picture: one call covers every scheduled event on that receiver at once, and costs no session. Its resolution is the spectrogram's, though, so the 4 dB dilution of a 3 kHz signal applies. It can detect a strong carrier or AM signal; it will not reliably detect a weak 3 kHz USB voice or a CW signal.

CONFIRMED LIVE caveat: M0LTE returned `204 No Content` for both noise-floor FFT requests at capture time. VERIFY the JSON shape on an instance that is producing data.

### 7.3 Other HTTP options

- `rowspectrum` (6.2) is the same data as the wideband FFT, but up to a minute old.
- `/api/noisefloor/spectrum/stream` (SSE, `main.go:3430`) and `/api/noisefloor/voice-activity` are amateur-band features. Not useful here.
- No HTTP endpoint gives power at an arbitrary frequency at better than 7.3 kHz resolution.

### 7.4 Recommended probe strategy

1. **Cheap and first:** one `/api/noisefloor/fft/wideband` per candidate receiver per probe cycle (at most 1 per 2 s per receiver, which a 60 s probe cycle is far inside). Compare the bins at the station's frequency against bins 3 to 5 away. That catches strong AM and carrier signals and costs no session slot.
2. **Precise, when needed:** a short spectrum session. Mint a UUID, `POST /connection`, open `/ws/user-spectrum?...&frequency=<f>&bin_bandwidth=10` (10.24 kHz span on 1024 bins), read 1 to 2 s of frames (10 to 20 frames; the first is a full frame), close with a close frame. Measure the peak within the expected passband (for example f..f+3 kHz for USB, f+/-300 Hz for CW) against the median of the rest. Hard timeout 10 s.
   - Cost per probe: 1 of 10 `/connection` tokens per minute, 1 WebSocket connection token, 1 spectrum session-creation token (6/min, burst 3 per UUID), and **one unique-user slot for the duration**. That slot also counts against `max_sessions_ip`, which the example config sets to 2. A probe can therefore collide with relay channels on the same receiver. Schedule probes so that probes plus channels never exceed the per-receiver cap.
   - A probe UUID can be reused for further probes within 5 minutes of its last close without a new `/connection` (section 2.3). Rotate it well before `max_session_time` (counted from first use) to avoid being kicked mid-probe.
3. **History:** `timeslice` (6.3) for "did it already start?".

## 8. `GET /audio/stream?session=<uuid>`: WebM muxing reference

`audio_http_stream.go`. Route `main.go:3522-3537`.

### 8.1 What the endpoint is

- It needs an active `/ws` audio session for the UUID; otherwise 404 (`audio_http_stream.go:249-258`). IQ modes get 409 (`:276-279`).
- While it is open, the server **diverts audio from the WebSocket to this response** (`websocket.go:2316-2361`). The WebSocket keeps sending signal-quality frames only (version >= 2). `DELETE /audio/stream?session=` diverts audio back to the WebSocket (`audio_http_stream.go:425-449`).
- It **re-encodes** from PCM with its own encoder. It is not a remux of the WebSocket Opus.
- Headers: `Content-Type: audio/webm; codecs=opus`, `Cache-Control: no-cache, no-store`, `X-Accel-Buffering: no`, `Access-Control-Allow-Origin: *` (`:328-335`). It flushes after every cluster.
- If the sample rate changes (a mode change between 12 kHz and 24 kHz modes), it ends the response and expects the client to reconnect (`:361-388`).

The relay could in principle pipe this upstream stream instead of muxing its own, but it is an Android lock-screen feature, it turns off Opus on the WebSocket, and its timecodes start at 0 per response. The brief's plan, remuxing WebSocket Opus ourselves, is the better fit.

### 8.2 Byte layout UberSDR emits (`audio_http_stream.go:101-198`)

Header, sent once:

```
EBML (1A 45 DF A3)
  EBMLVersion (42 86) = 1
  EBMLReadVersion (42 F7) = 1
  EBMLMaxIDLength (42 F2) = 4
  EBMLMaxSizeLength (42 F3) = 8
  DocType (42 82) = "webm"
  DocTypeVersion (42 87) = 4
  DocTypeReadVersion (42 85) = 2
Segment (18 53 80 67), size = unknown: 01 FF FF FF FF FF FF FF
  Info (15 49 A9 66)
    TimestampScale (2A D7 B1) = 1000000   (1 ms per tick)
    MuxingApp (4D 80), WritingApp (57 41) = "UberSDR"
  Tracks (16 54 AE 6B)
    TrackEntry (AE)
      TrackNumber (D7) = 1
      TrackUID (73 C5) = 1
      TrackType (83) = 2 (audio)
      CodecID (86) = "A_OPUS"
      CodecPrivate (63 A2) = OpusHead, 19 bytes:
        "OpusHead", version 1, channel count, pre-skip u16 LE = 0,
        input sample rate u32 LE = 48000, output gain i16 LE = 0, mapping family 0
      CodecDelay (56 AA) = 0
      SeekPreRoll (56 BB) = 0
      Audio (E1)
        SamplingFrequency (B5) = 48000.0 as 4-byte big-endian float (47 BB 80 00)
        Channels (9F) = channel count
```

No SeekHead, Cues, Duration or Tags. Then, for **every Opus packet**, one complete Cluster:

```
Cluster (1F 43 B6 75), known size
  Timecode (E7) = cumulative milliseconds (EBML unsigned, minimal bytes, big-endian)
  SimpleBlock (A3)
    track number VINT = 0x81, relative timecode i16 big-endian = 0, flags = 0x80 (keyframe), Opus packet
```

The timecode advances by the PCM duration of the packet (20 ms) computed from sample counts, not from the wall clock (`audio_http_stream.go:403-409`). Element sizes use the shortest VINT; values >= 0x7F take two bytes, so 1-byte sizes never use the reserved all-ones pattern (`:57-71`).

### 8.3 Guidance for our muxer

1. **Header.** Copy the layout above. OpusHead `input sample rate` is informational; 48000 is fine whatever rate the upstream encoder ran at (12 or 24 kHz). WebM Opus is always decoded at 48 kHz, so `SamplingFrequency` 48000.0 is correct. Channels = 1. Pre-skip and CodecDelay 0 are acceptable for a live stream: the cost is about 6.5 ms of encoder warm-up audio at the start.
2. **Timing.** Derive each packet's duration from its Opus TOC byte (RFC 6716 section 3.1) and keep the running position in 48 kHz samples; convert to ms for the timecodes. Never use the upstream header timestamps for this: the first one can be 0 (CONFIRMED LIVE), they jitter by microseconds, and the server's silence ticker uses a different clock (section 3.4). The upstream stream has no gaps (silence is encoded, not omitted), so advancing by packet duration keeps audio and timeline in step. If the upstream ever drops a packet, just carry on; the browser buffer absorbs 20 ms.
3. **Cluster shape.** One cluster per packet, as UberSDR does, is simple, flushes immediately, and is proven in Chrome. It costs about 12 to 14 bytes per 20 ms (about 0.6 kB/s). Larger clusters (several SimpleBlocks with relative timecodes, a new cluster at least every 32767 ms) save bytes but add latency unless the cluster is written with unknown size. VERIFY unknown-size clusters in Firefox and Safari before using them; the per-packet form is the safe default.
4. **Fan-out and late joiners.** Keep one muxer state per browser response, sharing the upstream Opus payloads. Send each new listener the header, then clusters with timecodes **starting at 0 for that listener**. Clusters are rebuilt per listener, which is cheap, and this avoids the `<audio>` element starting at a large `currentTime` or waiting for a gap.
5. **Mode or rate changes.** Not needed, because a channel is a fixed (frequency, mode). If a rate change ever happened, Opus packets at 12 kHz and 24 kHz internal rates are all valid input to one 48 kHz decoder, so a remuxer does not have to restart. UberSDR restarts only because its encoder is fixed.
6. **Response headers.** `Content-Type: audio/webm; codecs=opus`, `Cache-Control: no-cache, no-store`, `X-Accel-Buffering: no`. Flush after every cluster, and disable response buffering and compression for this route.

## 9. Constraints on a relay that sends everything from one IP

### 9.1 Per-IP and per-UUID limits

Defaults are code defaults; "example" values are what `config/config.yaml.example` ships, which is likely to be what most instances run.

| Limit | Key | Default | Scope | Failure | Code |
|---|---|---|---|---|---|
| `/connection` calls | `sessions_per_minute` | 10/min, burst 10 | per IP | 429 | `ratelimit.go:508-541`, `config.go:1357` |
| WebSocket upgrades (audio + spectrum share one bucket) | `conn_rate_limit` | 4/s, burst 4 (minimum 4) | per IP | HTTP 429 before upgrade | `ratelimit.go:154-176`, `config.go:1350-1356`, `main.go:2392-2395` |
| New sessions (reconnect churn) | `session_create_rate_limit`, `session_create_burst` | 6/min, burst 3, separately for audio and spectrum | per UUID | post-upgrade error "too many session attempts..." | `ratelimit.go:585-650`, `config.go:1362-1367`, `session.go:551-567` |
| Commands on a socket | `cmd_rate_limit` | >= 50/s | per UUID per socket kind | `error` with status 429, socket stays open | `config.go:1333-1335`, `websocket.go:925-934` |
| Unique users | `max_sessions` | 50 code / 20 example | global | 503 at `/connection`; error at session create | `config.go:1326-1328`, `session.go:647-664` |
| **Unique UUIDs per IP** | `max_sessions_ip` | 0 (unlimited) code / **2 example** | per IP | 503 at `/connection`; error at session create | `session.go:3069-3091`, `session.go:673-695` |
| Inactivity | `session_timeout` | 0 (off) | per UUID | kick + 1 h blacklist | section 3.6 |
| Lifetime | `max_session_time` | 0 code / 3600 example (M0LTE 3600, CONFIRMED LIVE) | per UUID from first sight | kick + 1 h blacklist | section 3.6 |
| Daily time | `max_daily_time_per_ip` | 0 (off) | per IP, rolling 24 h; each concurrent UUID ticks separately, so 3 channels for 10 min use 30 min | 429 at `/connection`; error at session create; live sessions kicked by a 30 s sweep | `ip_daily_time.go:15-31`, `session.go:2537-2588` |
| Spectrum-only | `spectrum_only_timeout` | 0 code / 60 example | per UUID | kick + 1 h blacklist | section 4.6 |
| Spectrogram and FFT HTTP | fixed | see 6.2 and 7 | per IP per key | 429 | `ratelimit.go:236-305` |

Of these, `/api/description` publicly exposes only `max_clients`, `available_clients` and `max_session_time` (CONFIRMED LIVE). `max_sessions_ip` and the timeouts are not published. **Assume `max_sessions_ip` = 2**, which matches the brief's per-receiver cap of 2, and count probes against it. On a 503 "per IP" answer, back off from that receiver.

**Etiquette point for Tom to decide:** `max_session_time` exists for "fair access and preventing long-term monopolisation" (`config/config.yaml.example:251-256`). The official UI mints a new UUID only when the person presses Listen again (`static/v2/src/radio/session.js:72-87`). An automatic reconnect under a fresh UUID whenever the hour runs out would make our relay the one client that never yields. The brief says "reconnect only if listeners remain". The more faithful equivalent is to end the channel at expiry and have the browser offer "Session time reached on <receiver>: continue?", so a person re-presses Play. Either way, never retry a kicked UUID (it is blacklisted for 1 h), and never reconnect with no listeners.

### 9.2 CORS (`main.go:345-441`)

- With `enable_cors: true`, every response reflects the request's `Origin` (or the origin of its `Referer`) in `Access-Control-Allow-Origin`, with `Access-Control-Allow-Credentials: true`, methods `GET, POST, PUT, DELETE, OPTIONS`, headers `Content-Type, Authorization`, and `Max-Age` 86400. `OPTIONS` gets 204 (`main.go:420-438`).
- With it off, CORS headers are still added for an origin whose hostname equals the instance-reporting hostname (`instances.ubersdr.org`, `main.go:390-399`), and for `/connection` only, from loopback origins (`main.go:401-419`).
- `rowspectrum`, `timeslice`, `allrows` and `/audio/stream` always send `Access-Control-Allow-Origin: *` (`spectrogram_recorder.go:3101`, `:3298`, `:3472`; `audio_http_stream.go:334`).
- The WebSocket upgrader accepts any Origin (`websocket.go:122-130`). But both sockets need a UUID registered through `/connection`, so browser-direct use still depends on `/connection` passing CORS.
- CONFIRMED LIVE: M0LTE reports `cors_enabled: true` in `/api/description`, so it can test the browser-direct path.
- Server-to-server requests send no `Origin`, so none of this affects the relay.

### 9.3 What the server trusts for the client IP (`main.go:5296-5375`)

`getClientIP` uses the TCP peer address unless the peer is a configured tunnel server or trusted proxy (`server.trusted_proxy_ips`, `trusted_containers`). Only then does it read `X-Real-IP`, and failing that the first entry of `X-Forwarded-For`. Our relay will never be in those lists, so those headers are ignored. **Do not send `X-Forwarded-For` or `X-Real-IP` at all.** Every listener behind the relay is, correctly, our one IP to the operator.

### 9.4 Bans

- IP bans are manual: admin UI, Telegram bot, WebSDR sysop (`admin.go:3271`, `admin.go:3320`, `telegram_bot_commands.go:308`, `websdr_websocket.go:2540`). The code does no automatic banning for rate-limit violations. Answers: 403 JSON on HTTP, 401 on the WebSocket upgrade, 403 at `/connection`.
- User-Agent regex bans: 403 (section 2.2).
- Country and ASN bans: 403, with an HTML page for ASN (`main.go:5513-5560`). **A relay hosted in a cloud or datacenter network may be ASN-banned on some instances.** Treat any 403 as "exclude this receiver for a long time" and record it.
- A kicked UUID: 410 at `/connection`, error on the sockets, for 1 hour.

### 9.5 Things never to send

`password` (operator bypass), `X-Forwarded-For`/`X-Real-IP`, wide IQ modes, non-JSON frames, a `User-Agent` that hides who we are.

## 10. Live verification (M0LTE, 2026-10-06 21:59 UTC)

Sequence, one connection at a time, about 12 s of live time in total:

1. `GET /api/description`: 200, version 0.1.66, `max_clients` 20, `available_clients` 19, `max_session_time` 3600, `cors_enabled` true, `tuning_range` 10000 to 30000000 Hz, `spectrum_poll_period` 100.
2. `GET /api/spectrogram/meta`: 200, 119 KB, `max-age=60`, 4096 bins x 7324.22 Hz, 0 to 30 MHz, 926 rows, `row_interval_seconds` 60.
3. `GET /api/spectrogram/timeslice?freq_hz=7910000&bandwidth_hz=3000&rolling=1`: 200, 5.4 KB, `bins_averaged` 2, 75 rows ending 21:03 UTC (recorder had no data after that).
4. `GET /api/noisefloor/fft/wideband` and `GET /api/noisefloor/fft?band=40m`: both **204** with empty body.
5. `POST /connection`: 200 `{allowed:true, session_timeout:3600, max_session_time:3600, bypassed:false, allowed_iq_modes:["iq48"], daily_time_used_secs:0, daily_time_remaining_secs:-1}`.
6. `WS /ws?frequency=7910000&mode=usb&bandwidthLow=50&bandwidthHigh=2700&format=opus&version=4`: upgrade in 0.06 s, 251 Opus frames in 5 s, `get_status` answered with `status` (sampleRate 12000, channels 1), `ping` answered with `pong` with `serverTimeNs`. Clean close 1000.
7. Same with `version=3`: 100 frames in 2 s with the 21-byte header, consecutive timestamps exactly 20 ms apart. Reusing the same UUID for a second audio socket was accepted. Clean close.
8. `WS /ws/user-spectrum?mode=binary8&version=2&frequency=7910000&bin_bandwidth=10`: gzip `config` first (1024 bins, 10 Hz, 10240 Hz span), a full 0x05 frame 0.1 s later, then a 0x06 delta every 100 ms. A `zoom` to 20 Hz/bin was answered with a new `config` (span 20480) and **no full frame** followed. Clean close.

Observed details are recorded against the relevant sections above and in `tests/fixtures/ubersdr/README.md`.

## 11. Open items (VERIFY)

1. radiod's `cwu`/`cwl` presets put an on-frequency carrier at a 500 Hz audio tone, and how `cwl` behaves (5.2). Check by ear or by FFT of decoded audio on a known carrier.
2. Folding below -500 Hz in CW modes, which decides the safe lower `bandwidthLow` (5.2).
3. Priyom's frequency convention for RTTY/FSK, centre or mark (5.3). The recipe works for both, but the waterfall marker should sit in the right place.
4. Spectrum raw-order rotation and the exact bin-centre formula, checked against a known carrier (4.5).
5. Whether off-ladder `bin_bandwidth` values are accepted at connect time (4.2).
6. `/api/noisefloor/fft/wideband` JSON on an instance currently producing data. M0LTE returned 204 (7.2).
7. Exact top-level shape of `GET /api/bands`, for the blocked-range check (5.4).
8. Unknown-size WebM clusters in Firefox and Safari, if we want fewer, larger clusters (8.3).
9. Real `max_sessions_ip`, `session_timeout` and `spectrum_only_timeout` values on public instances. They are not exposed; behave as if they were 2, non-zero and 60 s.

## 12. Notes from the Go implementation (`internal/provider/ubersdr`)

- **Force HTTP/1.1 on the session transport, including TLS ALPN.** Clearing `TLSNextProto` on a cloned `http.Transport` is not enough: a clone of a transport that has been used (as `http.DefaultTransport` always has) still advertises `h2` in `TLSClientConfig.NextProtos`. M0LTE's front end (Caddy) then picks HTTP/2, and the HTTP/1 client fails on `/connection` with "malformed HTTP response" (CONFIRMED LIVE). Set `NextProtos` to `["http/1.1"]` as well. This is a client-side trap, not an UberSDR behaviour.
- **Directory shape** (CONFIRMED LIVE, 2026-10-06, 53 instances; trimmed sample in `tests/fixtures/ubersdr/directory-instances.json`): `host`, `port` and `tls` are present on every entry; `tls` is omitted, not false, for plain-HTTP instances (seen on ports 80, 8080 and 9080); one instance uses HTTPS on 9443. `snr_1_8_30_mhz` and `snr_0_30_mhz` are -1 when not measured. `tuning_range` carries `min_frequency`, `max_frequency`, `spectrum_span_hz`, `spectrum_center_hz`, `input_samprate`. `load_status` was one of `ok`, `warning`, `critical`. `country_code` is lowercase.
- **The directory can disagree with the instance.** M0LTE's directory entry said `cors_enabled: false` while its own `/api/description` said `true` minutes earlier. Treat directory flags as hints.
- **Telling a kick from a dropped connection.** Both look like a TCP close with no close frame. After an unexpected end, one `POST /connection` with the same UUID answers 410 if the UUID was kicked (section 2), which the provider reports as a `RejectedError`; an end within 15 s of `max_session_time` is reported as the time limit without asking.
- **Second live run** (integration test, 2026-10-06 22:26 UTC): `bin_bandwidth=20` at connect gave 20 Hz/bin rows centred on 7910000 Hz straight away (no zoom needed); 250 Opus packets and 49 spectrum rows arrived in 5.1 s; both sockets closed cleanly. `/api/noisefloor/fft/wideband` still answered 204.
