#!/usr/bin/env bash
# Build and deploy to an LXC/VM over ssh: frontend, static binary, systemd unit.
#   scripts/deploy.sh [root@host]     (default root@10.45.0.26, the proxmox1 CT 151)
set -euo pipefail
target="${1:-root@10.45.0.26}"
here="$(cd "$(dirname "$0")/.." && pwd)"
cd "$here"

version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
( cd web && npm ci --silent && npm run build --silent )
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/m0lte/numbers-station-listener/internal/config.Version=${version}" -o bin/nsl ./cmd/nsl

ssh "$target" 'id nsl >/dev/null 2>&1 || useradd --system --home /var/lib/nsl --shell /usr/sbin/nologin nsl; mkdir -p /opt/nsl /etc/nsl'
scp -q bin/nsl "$target:/opt/nsl/nsl.new"
scp -q deploy/nsl.service "$target:/etc/systemd/system/nsl.service"
ssh "$target" 'test -f /etc/nsl/nsl.env' || scp -q deploy/nsl.env.example "$target:/etc/nsl/nsl.env"
ssh "$target" 'mv /opt/nsl/nsl.new /opt/nsl/nsl && chmod 755 /opt/nsl/nsl && systemctl daemon-reload && systemctl enable --now nsl >/dev/null 2>&1; systemctl restart nsl'
for _ in $(seq 1 30); do
  if ssh "$target" 'curl -fsS http://127.0.0.1:8080/healthz' >/dev/null 2>&1; then
    echo "deployed ${version} to ${target}"
    exit 0
  fi
  sleep 1
done
echo "service did not become healthy; recent log:" >&2
ssh "$target" 'journalctl -u nsl -n 40 --no-pager' >&2
exit 1
