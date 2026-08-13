# mqvpn-prometheus-exporter

Sidecar Prometheus exporter for the [mqvpn](https://github.com/mp0rta/mqvpn)
multipath QUIC VPN server. Polls mqvpn's JSON control API and exposes
`/metrics`.

## Install

> **Want monitoring components on container?** 
> A pre-wired `docker compose` stack
> (exporter + Prometheus + Grafana, datasource
> and dashboard auto-provisioned) is at [`examples/compose/`](examples/compose/README.md). Linux only.

Pre-built static binaries (amd64 / arm64) are attached to
each [GitHub Release](https://github.com/mp0rta/mqvpn-prometheus-exporter/releases).
Pick the archive matching your CPU.
(`mqvpn-prometheus-exporter_<version>_linux_<arch>.tar.gz`), then:

```
tar xzf mqvpn-prometheus-exporter_*.tar.gz
sudo install mqvpn-prometheus-exporter /usr/local/bin/
```

Or build from source:

```
go install github.com/mp0rta/mqvpn-prometheus-exporter@latest
```

## Quickstart

```
mqvpn-prometheus-exporter --mqvpn.address 127.0.0.1:9090 --web.listen-address 127.0.0.1:9091
```

See [docs/README.md](docs/README.md) for the full operator guide
(deployment topology, Prometheus + Grafana setup, metric reference,
troubleshooting).

## What's in the repo

- [`docs/README.md`](docs/README.md) — operator guide
- [`examples/compose/`](examples/compose/README.md) — one-command
  Prometheus + Grafana + exporter stack (Linux only, `network_mode: host`)
- [`examples/systemd/`](examples/systemd/) — systemd unit for the exporter
- [`examples/prometheus.yml`](examples/prometheus.yml) — sample scrape config
- [`dashboards/mqvpn-grafana.json`](dashboards/mqvpn-grafana.json) — bundled
  Grafana dashboard (six rows: server overview, per-user / per-path,
  backup-FEC, reorder buffer, hybrid TCP lane, UDP offload)

## Compatibility

| Exporter | mqvpn                                     |
|----------|-------------------------------------------|
| 0.1.0    | ≥ 0.5.0                                   |
| 0.2.0    | ≥ 0.5.0 (reorder metrics ≥ 0.8.0)         |
| 0.3.0    | ≥ 0.5.0 (reorder ≥ 0.8.0, hybrid ≥ 0.9.0) |
| 0.4.0    | ≥ 0.5.0 (reorder ≥ 0.8.0, hybrid ≥ 0.9.0, reinject ≥ 0.15.0, udp offload ≥ 0.16.0) |

On mqvpn < 0.8.0 the `mqvpn_reorder_*` metrics are silently omitted; the
exporter functions normally otherwise. The `mqvpn_hybrid_*` metrics are
additive `get_stats` fields, so on mqvpn < 0.9.0 they read 0 rather than being
omitted. The `mqvpn_udp_*` pair and `mqvpn_path_reinject_tx_bytes_total` are
likewise additive (mqvpn ≥ 0.16.0 / ≥ 0.15.0) and read 0 on older servers
(the per-path reinject series exists only while a client is connected).
See the [compatibility notes](docs/README.md#9-mqvpn-version-compatibility)
for RPC-level detail.

## License

Apache License 2.0

Copyright (c) 2026 mp0rta

## Development

The repository ships with an opt-in [gitleaks](https://github.com/gitleaks/gitleaks)
pre-commit hook to prevent accidental secret leaks. To enable:

```
go install github.com/zricethezav/gitleaks/v8@latest
git config core.hooksPath hooks
```

Without this, `git commit` works as usual (the hook is opt-in via `core.hooksPath`).
With it enabled, every commit is scanned and rejected on a hit.

Run tests locally with:

```
go vet ./...
go test -race -count=1 ./...
```

CI runs the same on every push and PR (see `.github/workflows/ci.yml`).
