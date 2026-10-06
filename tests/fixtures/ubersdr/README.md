# UberSDR captured fixtures

Captured live on 2026-10-06 at about 21:59 UTC from Tom's own instance, https://reading-ubersdr.m0lte.uk/ (callsign M0LTE, UberSDR version 0.1.66), with the User-Agent `numbers-station-listener/dev (+https://github.com/m0lte/numbers-station-listener; M0LTE)`. Every socket was opened one at a time and closed with a normal close frame (code 1000). Total live time was about 12 seconds.

The protocol these exercise is written up in `docs/ubersdr-protocol.md`.

## Files

| File | What it is |
|---|---|
| `api-description.json` | `GET /api/description`, verbatim. |
| `directory-instances.json` | `GET https://instances.ubersdr.org/api/instances?online_only=true` on 2026-10-06 at about 22:11 UTC, trimmed to four instances (M0LTE; M0EYT, plain HTTP on 9080 with `tls` absent; K1RA, HTTPS on 9443; NA5B, `antenna_connected: false`) and with the bulky `ssb_predictions`, `ssb_grid_squares`, `pskreporter_rank`, `enabled_widgets`, `addons`, `first_seen` and `successful_callbacks` fields removed. Everything kept is verbatim. |
| `connection-response.json` | `POST /connection` response body. `client_ip` has been replaced with `REDACTED`; everything else is verbatim. Note `session_timeout` 3600 is the server echoing `max_session_time` because the real inactivity timeout is 0 (disabled). |
| `api-spectrogram-meta.json` | `GET /api/spectrogram/meta` (today, not rolling), verbatim. 926 rows; the per-row `noise_floor` and `peak_db` are all 0 on this instance at capture time. |
| `api-spectrogram-timeslice-7910k.json` | `GET /api/spectrogram/timeslice?freq_hz=7910000&bandwidth_hz=3000&rolling=1`, verbatim. Only 75 rows, ending 21:03 UTC: rows with no data are skipped, and the recorder had no FFT data after 21:03. |
| `audio-opus-v4-7910k-usb.rec` | Every WebSocket message from `/ws?frequency=7910000&mode=usb&bandwidthLow=50&bandwidthHigh=2700&format=opus&version=4` for 5 seconds: 251 binary Opus frames plus 2 text frames (a `status` reply to `get_status` and a `pong`). |
| `audio-opus-v3-7910k-usb.rec` | Every message from `/ws?frequency=7910000&mode=usb&format=opus&version=3` for 2 seconds: 100 binary frames with the fixed 21-byte header. |
| `spectrum-v2-binary8-7910k.rec` | The first 30 messages from `/ws/user-spectrum?mode=binary8&version=2&frequency=7910000&bin_bandwidth=10`: a gzip `config` message, one full frame (flags 0x05), deltas (0x06), then a second gzip `config` after a `zoom` to 20 Hz/bin, then more deltas. Note that no full frame follows the zoom. |

The `/api/noisefloor/fft/wideband` and `/api/noisefloor/fft?band=40m` requests both returned `204 No Content` with an empty body at capture time, so there is no fixture for them.

## `.rec` layout

A `.rec` file is a plain concatenation of records, in the order they were received. Each record is:

| Offset | Size | Type | Meaning |
|---|---|---|---|
| 0 | 1 | u8 | Message kind: 0 = binary WebSocket message, 1 = text WebSocket message (UTF-8) |
| 1 | 4 | u32 little-endian | Receive time in milliseconds since the socket opened |
| 5 | 4 | u32 little-endian | Payload length N in bytes |
| 9 | N | bytes | The WebSocket message payload, exactly as received |

There is no file header and no padding. The record wrapper is ours; the payloads are UberSDR's.

## What the captures confirmed

- Audio v4 Opus frames: first byte is the flags byte (0x00 to 0x03). The very first frame carried a timestamp of 0, then the second frame resynchronised with the real GPS time; after that the timestamp deltas are 20,000,000 ns (20 ms) apart. 2 of 251 frames had the metadata flag, 52 had the quality flag, 197 had neither. Average header 5.9 bytes.
- Every Opus payload has TOC config 5: SILK, mediumband, 20 ms frame, mono, one frame per packet, at a 12 kHz session rate. Payloads were 97 to 129 bytes (about 110 average).
- Audio v3 frames: 21-byte header, u64 timestamp, u32 sample rate 12000, u8 channels 1, f32 baseband power, f32 passband noise power (about -86 and -93 to -97 dBFS here), then Opus.
- Spectrum v2: 1024 bins at 10 Hz/bin (10240 Hz span), scale ref -145.33 dB, step 0.25 dB on the first full frame, 10 frames per second, delta masks always 128 bytes with one value byte per set bit.
