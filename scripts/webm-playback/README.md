# WebM playback check

Plays `internal/webm` output in real browsers. This is a manual check, not part of `go test`, because it uses real time and real browsers.

Run it from the repo root:

```
(cd scripts/webm-playback && npm install)
eval "$(scripts/webm-playback/pulse-null.sh)"   # optional: gives headless Firefox an audio device
node scripts/webm-playback/check.mjs
kill "$NSL_PULSE_PID"
```

`check.mjs` builds and starts `serve/` (Go, uses the UberSDR Opus capture in `tests/fixtures/ubersdr/`), then for each browser it can launch (Playwright Chromium from `~/.cache/ms-playwright`, `/usr/bin/google-chrome`, Playwright Firefox) loads each stream into an `<audio>` element, calls `play()` and waits for `currentTime` to pass a target with no `MediaError`. It also plays the live stream through Media Source Extensions. It exits non-zero on any failure. Install Firefox with `npx playwright-core install firefox` from this directory.

`go run ./scripts/webm-playback/serve -out sample.webm` just writes a 6 s sample file.

Streams served: `/static.webm` (complete file), `/silence.webm` (CELT silence), `/live.webm` (endless, chunked, paced in real time; query `burst=N` sends N packets at once first, `src=silence` uses 3-byte packets, `cluster=100ms` sets the cluster length).
