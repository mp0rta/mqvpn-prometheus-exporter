# mqvpn-prometheus-exporter

Sidecar Prometheus exporter for the [mqvpn](https://github.com/mp0rta/mqvpn)
multipath QUIC VPN server. Polls mqvpn's JSON control API and exposes
`/metrics`.

> **Want everything in one command?** A pre-wired `docker compose` stack
> (exporter + Prometheus + Grafana, datasource and dashboard
> auto-provisioned) is at
> [`examples/compose/`](examples/compose/README.md). Linux only.

## Install

Pre-built static binaries (linux / darwin × amd64 / arm64) are attached to
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
  Grafana dashboard (server overview, per-user / per-path, backup-FEC)

## Compatibility

| Exporter | mqvpn       |
|----------|-------------|
| 0.1.x    | ≥ 0.5.0     |

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
