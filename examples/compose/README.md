# Compose Stack — Self-host Observability for mqvpn

One-command Prometheus + Grafana + exporter stack for the mqvpn server.
Suitable for personal self-host production as well as development.
**Linux only** — relies on `network_mode: host`.

This is a peer alternative to the systemd setup in `docs/README.md` §4.2;
both bind every service to loopback. Pick this stack if you want
configuration-as-code, easy host-to-host portability, dependabot-driven
image updates, and fast wipe-and-rebuild semantics; pick systemd if you
prefer distro packages with `unattended-upgrades` and don't want a Docker
daemon on the host.

## What's included

| Service | Bind | Notes |
|---------|------|-------|
| `exporter` | `127.0.0.1:9091` | Built from this repo via `Dockerfile`. Distroless image, ~17 MB, runs as nonroot. |
| Prometheus | `127.0.0.1:9092` | Scrapes `exporter:9091` (via host loopback) and itself. 24h tsdb retention by default. |
| Grafana | `127.0.0.1:3000` | Datasource + dashboard auto-provisioned; UI edits allowed. |
| `dashboards-init` | — | One-shot busybox; substitutes `${DS_PROMETHEUS}` in the bundled dashboard JSON before Grafana loads it. |

Prometheus and Grafana have `healthcheck:` configured. The exporter
container relies on `restart: unless-stopped` plus
`up{job="mqvpn"}` from Prometheus — distroless has no shell or
HTTP-probe binary, and the exporter has no self-probe mode.

**Not included:** `mqvpn` itself. `mqvpn` is a VPN server requiring
`CAP_NET_ADMIN`, a TUN device, and multipath-aware kernel routing — out
of scope for containerisation. Run it on the host with its control API
on `127.0.0.1:9090`.

## Quickstart

```bash
# 1. on the VPN host: ensure mqvpn is running with its control API on
#    127.0.0.1:9090. Any launch method works (install script, INI/JSON
#    config file, systemd, or direct CLI) — see docs/README.md §1.1 or
#    the upstream mqvpn README. The control API is off by default; you
#    must enable it explicitly.

# 2. start the full stack (builds the exporter image on first run)
cd examples/compose
docker compose up -d --build

# 3. from your laptop: tunnel and browse
ssh -L 3000:127.0.0.1:3000 vpn-host
# open http://localhost:3000  (admin / admin — change on first login)
```

## Updating

Image tags are pinned. To pick up new Prometheus / Grafana / busybox
releases, either:

- Merge the dependabot PRs that target this directory (monthly cadence),
  then `docker compose pull && docker compose up -d` on the host
- Or manually bump the tags in `docker-compose.yml` and rebuild

To rebuild the exporter image after changing source code:

```bash
docker compose up -d --build exporter
```

## Restarts vs wipes

```bash
# Restart (preserves Prometheus tsdb + Grafana state):
docker compose restart
docker compose down && docker compose up -d

# Wipe state and start fresh — irreversible:
docker compose down -v
```

`-v` drops the named volumes (`prometheus-data`, `grafana-data`,
`grafana-dashboards`). Useful when you want a clean slate; destructive
otherwise. Don't make it muscle memory.

## Iterating on exporter source code

When changing exporter Go code, rebuilding the container per change is
slow. Bring up only the observability services and run the exporter from
source on the host instead:

```bash
docker compose up -d dashboards-init prometheus grafana
# in another shell, on the host:
go run . --web.listen-address 127.0.0.1:9091 --mqvpn.address 127.0.0.1:9090
```

Prometheus is host-networked, so it reaches `127.0.0.1:9091` regardless of
whether the exporter runs in a container or directly on the host.

## Why `network_mode: host`?

The exporter and mqvpn's control API both bind to `127.0.0.1` (no auth,
loopback-only — see `docs/README.md` §10 Safety). Container loopback is a
separate namespace, so without host networking Prometheus inside a
container can't reach `127.0.0.1:9091` on the host. Two alternatives this
stack deliberately rejects:

1. **Bind the exporter to `0.0.0.0`** — would let the container reach it
   via a docker bridge, but the exporter has no auth and you'd be
   exposing it on the LAN.
2. **`extra_hosts: host.docker.internal:host-gateway` + bind exporter to
   the bridge IP** — same problem, plus more moving parts.

`network_mode: host` is Linux-only but matches the operator-guide
deployment topology exactly: everything on loopback, accessed via tunnel.

## Backup

Prometheus tsdb and Grafana state live in docker volumes under
`/var/lib/docker/volumes/compose_prometheus-data/_data` and
`/var/lib/docker/volumes/compose_grafana-data/_data`. Add those paths to
your backup tool, or take ad-hoc snapshots:

```bash
docker run --rm \
  -v compose_grafana-data:/data \
  -v $(pwd):/backup \
  busybox tar -czf /backup/grafana.tgz -C /data .
```

## Troubleshooting

- **Grafana shows "Datasource named ${DS_PROMETHEUS} was not found":**
  the `dashboards-init` service didn't run or the volume is stale. Run
  `docker compose down -v && docker compose up -d` to regenerate.
- **Prometheus targets page shows mqvpn down:** the exporter on the host
  isn't running or isn't on `127.0.0.1:9091`. Check `curl
  http://127.0.0.1:9091/metrics` from the host. If you're running the
  exporter outside the container (iteration mode), make sure the
  `exporter` compose service is stopped.
- **Healthcheck flapping:** `docker compose ps` shows `(unhealthy)`
  briefly during cold start (start_period 5–30s); persistent unhealthy
  state indicates a real problem — check `docker compose logs <svc>`.
- **Port 9092 / 3000 / 9091 already in use:** another process owns the
  port. `ss -tlnp | grep -E ':(3000|9091|9092)'` to find the offender.
