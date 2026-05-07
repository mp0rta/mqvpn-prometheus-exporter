# mqvpn-prometheus-exporter

Sidecar Prometheus exporter for the [mqvpn](https://github.com/mp0rta/mqvpn)
multipath QUIC VPN server. Polls mqvpn's JSON control API and exposes
`/metrics`.

## Quickstart

```
go install github.com/mp0rta/mqvpn-prometheus-exporter@latest
mqvpn-prometheus-exporter --mqvpn.address 127.0.0.1:9090 --web.listen-address 127.0.0.1:9091
```

See [docs/README.md](docs/README.md) for full operator documentation.

## Compatibility

| Exporter | mqvpn       |
|----------|-------------|
| 0.1.x    | ≥ 0.5.0     |

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
