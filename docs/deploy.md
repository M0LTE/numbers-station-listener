# Deployment

## Where it runs

| What | Where |
|---|---|
| Container | proxmox1 (10.45.0.10), CT 151 `numbers`, Debian 13, unprivileged, 2 cores, 1 GB, 8 GB disk, onboot |
| Address | 10.45.0.26 (DHCP), `numbers.lan`; site on port 8080, port 80 redirects to it |
| Service | systemd unit `nsl`, binary `/opt/nsl/nsl`, user `nsl`, state in `/var/lib/nsl` |
| Config | `/etc/nsl/nsl.env` (environment variables, see below) |
| Logs | `journalctl -u nsl` (plain ASCII) |

## Deploying

From a checkout with Go 1.25+ and Node 22:

```
scripts/deploy.sh                 # defaults to root@10.45.0.26
scripts/deploy.sh root@otherhost
```

It refuses to run with uncommitted changes, so whatever is deployed is in the repo. It builds the frontend and a static binary, copies them over, installs the unit, keeps an existing `/etc/nsl/nsl.env`, restarts, and waits for `/healthz`.

A Dockerfile is also provided (`docker build -t nsl . && docker run -p 8080:8080 -v nsl:/data nsl`) for hosting elsewhere.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `NSL_LISTEN` | `:8080` | Listen address |
| `NSL_REDIRECT_LISTEN` | | If set (the container uses `:80`), a plain 302 redirect to the same host on the `NSL_LISTEN` port |
| `NSL_PUBLIC_URL` | | The site's public URL; goes into the User-Agent |
| `NSL_CONTACT` | `M0LTE` | Contact in the User-Agent sent to receivers and Priyom |
| `NSL_DATA_DIR` | | Where the schedule snapshot is kept (so restarts do not need Priyom) |
| `NSL_RECEIVER_ALLOW` | | Comma list of callsigns, ids or hostnames. If set, the site only ever connects to or probes these. **Set to `M0LTE` until go-live.** |
| `NSL_GRACE` | `10s` | How long an upstream survives its last listener (max 30 s) |
| `NSL_PER_RECEIVER_CAP` | `2` | Most sessions we hold on one receiver (UberSDR's default per-IP cap is also 2) |
| `NSL_SPAN_HZ` | `12000` | Waterfall width |
| `NSL_PROBE` | `true` | Signal probing on or off |
| `NSL_PROBE_TOP_K` | `5` | Receivers probed per live transmission |
| `NSL_PROBE_EVERY` | `60s` | Probe round interval (floor 30 s) |
| `NSL_PRIYOM_POLL` | `15m` | Schedule poll interval (floor 5 m) |
| `NSL_DIRECTORY_POLL` | `5m` | Receiver directory poll interval (floor 1 m) |
| `NSL_RANK_WEIGHTS` | | JSON override, e.g. `{"probe":0.5,"path":0.3,"quality":0.1,"load":0.1}` |
| `NSL_ADMIN_NETS` | loopback + RFC 1918 | Who may see `/admin/sessions` and `/metrics` |
| `NSL_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `NSL_PSKR` | `true` | PSKReporter path-open hint (MQTT feed, about 20 MB of memory at most); adds a 0.1 ranking weight |
| `NSL_PRIYOM_URL`, `NSL_UBERSDR_DIRECTORY` | the real ones | Override for testing |

## Monitoring

- `GET /admin/sessions` (LAN only): every channel, its listeners and upstream state. `overdueUpstreams` must be 0.
- `GET /metrics` (LAN only), Prometheus text. Alert on `nsl_overdue_upstream_sessions > 0` and on any increase of `nsl_watchdog_closes_total` or `nsl_leaked_leases_total`: each means the relay held a receiver connection nobody was listening to, which is a bug.

Both refuse any request carrying Cloudflare headers, so they stay private after the tunnel goes live even though the tunnel connector is on the LAN.

## Public access via Cloudflare Tunnel (live since 2026-10-06)

The site is public at **https://numbers.m0lte.uk**.

- The tunnel connector is proxmox1 CT 103 (`cloudflared`, remotely managed with a token), tunnel `proxmox-cloudflared` (id `60065efc-b05b-4ce7-87a8-fc64833b1049`). No new connector was needed.
- Its ingress has `numbers.m0lte.uk -> http://numbers.lan:8080`, inserted before the catch-all 404. The hostname (not the IP) is used so a DHCP change does not break it; `numbers.lan` resolves from CT 103.
- DNS: proxied CNAME `numbers.m0lte.uk -> 60065efc-b05b-4ce7-87a8-fc64833b1049.cfargotunnel.com`.
- `/etc/nsl/nsl.env` has `NSL_PUBLIC_URL=https://numbers.m0lte.uk` and no receiver allow list (the pre-go-live copy is `/etc/nsl/nsl.env.pre-golive`).
- No cache rule was needed: the API and streams send `no-cache` or `no-store`, which Cloudflare honours, and audio, SSE and WebSockets were verified streaming through the tunnel.

To take it offline again: delete the ingress rule (Zero Trust, Networks, Tunnels, proxmox-cloudflared, Public Hostname) and the DNS record, or just `systemctl stop nsl`.

Things to know:

- Every upstream connection comes from this site's one IP, so per-IP limits on each receiver apply to all listeners combined. The relay shares one session per receiver and frequency among everyone and caps us at 2 per receiver.
- When a receiver's session time limit runs out we do not reconnect with a fresh session (that would sidestep the operator's limit); the player moves to the next receiver instead.
- Tell the Priyom team the site exists before publicising it (brief, milestone 0).
