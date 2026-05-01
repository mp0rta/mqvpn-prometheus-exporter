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
| 0.1.x    | ≥ 0.4.0     |
