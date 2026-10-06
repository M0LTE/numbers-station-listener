# HTTP API

The contract between the Go backend and the browser. All JSON uses camelCase. All times are RFC 3339 UTC strings. Frequencies are integers in Hz unless the field name says kHz.

## Shared shapes

### Receiver summary

```json
{
  "key": "ubersdr:b838fc45-8dd2-4fa8-bb0d-8670244ad5da",
  "provider": "ubersdr",
  "callsign": "M0LTE",
  "name": "SDR with Active Loop",
  "location": "Reading, England, UK",
  "country": "gb",
  "lat": 51.46, "lon": -0.98,
  "publicUrl": "https://reading-ubersdr.m0lte.uk/",
  "distanceKm": 9120,          // from the station's transmitter site, null if unknown
  "score": 0.73,               // 0..1, higher is better; only meaningful within one event
  "reasons": ["night path, 1-hop F2", "good receiver SNR", "18 free slots"],
  "availableClients": 18,
  "maxClients": 20,
  "deepLink": "https://reading-ubersdr.m0lte.uk/?freq=7910000&mode=usb"
}
```

### Signal

```json
{ "state": "unknown" | "present" | "absent", "snr": 14.2, "at": "2026-10-06T21:00:30Z", "receiverKey": "ubersdr:..." }
```

`snr` and `receiverKey` are absent when `state` is `unknown`.

### Event

One scheduled transmission from the Priyom feed.

```json
{
  "id": "e3f1a09b2c4d5e6f",                 // stable: hash of start, station, frequencies
  "station": "V13",                         // designator as Priyom gives it, variant suffix kept
  "stationName": "New Star Broadcasting Station",
  "priyomUrl": "https://priyom.org/number-stations/other/v13",   // null if not in the catalogue
  "language": "Chinese",
  "category": "other",
  "start": "2026-10-07T00:00:00Z",
  "end": "2026-10-07T00:20:00Z",            // estimated from the catalogue, or cut short by probes
  "endEstimated": true,                     // false once a probe has seen the carrier go
  "status": "upcoming" | "live" | "done",
  "search": false,                          // a "Search" entry: no frequency known
  "freqs": [
    {
      "hz": 15388000,
      "signal": { "state": "unknown" },
      "receivers": [ /* receiver summaries, best first, at most 3 */ ]
    }
  ],
  "priyomMode": "USB/AM",                   // as written in the feed
  "mode": "usb",                            // what we tune: usb lsb am sam cwu cwl
  "digital": false,                         // RTTY, FSK, etc: listen in USB, note in the UI
  "remarks": ["Target: East Asia", "In case of traffic"],
  "target": "East Asia",                    // from a "Target:" remark, null otherwise
  "raw": "V13 15388kHz USB/AM [Target: East Asia]",
  "parsed": true                            // false: shown with raw text only, no listening
}
```

## Routes

### `GET /api/now`

```json
{
  "serverTime": "2026-10-06T21:58:03Z",
  "scheduleUpdated": "2026-10-06T21:45:00Z",
  "now": [ Event, ... ],      // status live, soonest-ending first
  "next": [ Event, ... ],     // the next 8 upcoming, soonest first
  "later": [ Event, ... ]     // further upcoming events within the next 24 h
}
```

### `GET /api/schedule?from=&to=`

`{ "events": [ Event, ... ] }`, sorted by start. Defaults: from = start of today UTC, to = end of tomorrow UTC. Only what the server has cached; it never fetches Priyom per request.

### `GET /api/live` (Server-Sent Events)

- `event: now`, data: the same JSON as `GET /api/now`. Sent on connect and whenever anything in it changes (status, probe result, receiver ranking). At most once per 2 s.
- A comment line (`: ping`) every 25 s keeps proxies from closing the stream.

Holding this stream open is not listening and never opens an upstream session.

### `POST /api/channels`

Body, either:

```json
{ "eventId": "e3f1a09b2c4d5e6f", "freqHz": 15388000, "receiverKey": "ubersdr:..." }
```

(`freqHz` defaults to the event's first frequency, `receiverKey` to the best ranked receiver), or a free tune:

```json
{ "freqHz": 7910000, "mode": "usb", "receiverKey": "ubersdr:..." }
```

Response 200:

```json
{
  "channelId": "9a8b7c6d5e4f3a2b",
  "listenerId": "7d0c...",            // server-generated, use it for the three routes below
  "receiver": ReceiverSummary,
  "freqHz": 15388000,
  "mode": "usb",
  "spanHz": 12000,
  "capabilities": { "historicalSpectrogram": true, "liveSpectrum": true },
  "alternatives": [ ReceiverSummary, ... ]   // the other ranked receivers, best first
}
```

Creating a channel does **not** open anything upstream. Errors: 400 bad body, 404 unknown event or receiver, 409 the receiver cannot tune that frequency.

### `GET /listen/{channelId}/audio.webm?listener={listenerId}`

A live `audio/webm` (Opus) stream. This is what makes you a listener: the upstream session opens on the first such request (or spectrum socket) and closes 10 s after the last one ends.

If the upstream cannot be opened the response is an error with a JSON body instead of audio:

- 503 `{ "error": "receiver_busy" }`: we already hold the maximum sessions on that receiver.
- 503 `{ "error": "rejected", "reason": "..." }`: the receiver refused (full, rate limited, time limit).
- 502 `{ "error": "upstream", "reason": "..." }`: network or protocol failure.
- 404 `{ "error": "no_channel" }`: create the channel again.

On any of these, the frontend should try the next alternative receiver. The stream ends (EOF) if the receiver drops us for good; treat that the same way.

### `WS /listen/{channelId}/spectrum?listener={listenerId}`

Counts as listening, like the audio stream.

- First a text frame: `{ "type": "header", "startHz": 15382000, "binHz": 23.4, "bins": 512, "centerHz": 15388000, "tunedHz": 15388000, "dbMin": -130, "dbMax": -40 }`. A new header is sent whenever the geometry or scaling changes.
- Then binary frames, one per waterfall row: `bins` bytes, each `round((dB - dbMin) / (dbMax - dbMin) * 255)` clamped to 0..255, lowest frequency first.
- Text frames `{ "type": "error", "error": "...", "reason": "..." }` precede a close when the upstream fails.
- The server pings every 5 s and drops the socket after 15 s without a pong.

### `DELETE /api/channels/{channelId}/listeners/{listenerId}`

Explicit leave. Also accepted as `POST /api/channels/{channelId}/listeners/{listenerId}/leave`, because `navigator.sendBeacon` can only POST. Returns 204. The server never depends on it; it only speeds up release.

### `GET /listen/{channelId}/spectrogram.png?minutes=30`

Recorded wideband spectrogram of the last `minutes` (5 to 60) around the channel frequency, as a PNG. Served from a shared cache refreshed at most once per 60 s per receiver and frequency band; upstream is never asked more than once per 10 s. 404 if the provider has no history (`capabilities.historicalSpectrogram` false). Does not count as listening and opens no session.

### `GET /api/stations`

The station catalogue: `{ "stations": { "V13": { ... } }, "attribution": { ... } }`.

### `GET /api/stations/{designator}.ics`

iCal feed of that station's scheduled transmissions in the cached window.

### `GET /api/receivers`

`{ "receivers": [ ReceiverSummary without score/reasons/distanceKm/deepLink ], "updated": "..." }`.

### `GET /admin/sessions`

Relay state: every channel with upstream status and listener counts, plus counters. `overdueUpstreams` must always be 0. Restricted to the admin network (see deployment docs).

### `GET /metrics`

Prometheus text format.

### `GET /healthz`

`200 ok`.
