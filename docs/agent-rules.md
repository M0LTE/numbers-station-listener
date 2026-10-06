# Rules for anyone (human or agent) working in this repo

- Go 1.25+ module `github.com/m0lte/numbers-station-listener`. Frontend in `web/` (TypeScript + Vite, no framework), built into `internal/webui/dist`.
- Run `gofmt -w`, `go vet ./...` and `go test -race ./...` before handing work back. All must be clean.
- **No wall clock in tests.** No test may decide anything by real elapsed time: no `time.Sleep` against real time, no "wait up to N seconds". Use `testing/synctest` (fake clock bubble; production code just uses the `time` package) or pass times in explicitly. A test that hangs is an honest failure; a deadline is not.
- Tests and CI never touch public UberSDR instances or Priyom. Live checks against M0LTE only (`https://reading-ubersdr.m0lte.uk/`), gated behind `NSL_INTEGRATION=1`.
- Stdlib only unless a dependency clearly earns its place; say why in the hand-back.
- Never write an em dash or en dash anywhere (code, comments, docs, UI text). Use a hyphen, comma, semicolon or a new sentence. Anything printed to a log or terminal is plain ASCII.
- Shared foundation, do not edit without asking: `internal/model`, `internal/provider`, `internal/relay`, `internal/stations`, `docs/api.md`.
- UberSDR is GPL-3.0: read it for the protocol, never copy its code. Priyom content is CC BY-NC-SA 4.0: keep attribution.
- Do not git commit; the integrator does that.
