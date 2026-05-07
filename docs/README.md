# mqvpn-prometheus-exporter — Operator Guide

Sidecar Prometheus exporter for the [mqvpn](https://github.com/mp0rta/mqvpn)
multipath QUIC VPN server. Polls mqvpn's JSON control API and exposes
`/metrics` in Prometheus exposition format.

---

## 1. Quickstart

### 1.1 Start mqvpn server (with control API enabled)

The exporter only requires that mqvpn's control API is reachable at the
address you pass via `--mqvpn.address`. Pick whichever launch method fits
your existing setup; the canonical reference is the
[mqvpn README](https://github.com/mp0rta/mqvpn/blob/main/README.md).

```bash
# Install script — sets up config + cert + auth key, optionally starts the server.
# Pass --enable-control <port> to bind the control API (default port 9090).
curl -fsSL https://github.com/mp0rta/mqvpn/releases/latest/download/install.sh \
  | sudo bash -s -- --start --enable-control 9090

# Config file (INI) — add `Listen = 127.0.0.1:9090` under [Control], then:
sudo mqvpn --config /etc/mqvpn/server.conf

# Config file (JSON) — set `"control_listen": "127.0.0.1:9090"`, then:
sudo mqvpn --config /etc/mqvpn/server.json

# systemd — assumes /etc/mqvpn/server.conf exists with [Control] configured.
sudo systemctl enable --now mqvpn-server

# Direct CLI — useful for ad-hoc tests; the control API is off unless you ask.
sudo mqvpn --mode server --control-port 9090 --control-addr 127.0.0.1 ...
```

The control API is **disabled** by default; without one of the above
flags / config keys the exporter will fail every scrape with "connection
refused".

### 1.2 Run the exporter

```bash
go install github.com/mp0rta/mqvpn-prometheus-exporter@latest
mqvpn-prometheus-exporter --mqvpn.address 127.0.0.1:9090 --web.listen-address 127.0.0.1:9091
```

Open `http://127.0.0.1:9091/metrics` to verify output.

> **Want exporter + Prometheus + Grafana in one command?** Skip §1.2 and
> use the bundled docker compose stack at
> [`examples/compose/`](../examples/compose/README.md) — pre-provisioned
> datasource, auto-loaded dashboard, healthchecks. See §4.5 for the
> rationale and tradeoffs versus the systemd setup in §4.2.

---

## 2. CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--web.listen-address` | `127.0.0.1:9091` | Address on which to expose `/metrics`. Defaults to loopback. Set to `0.0.0.0:9091` to expose externally — you MUST front with nginx for authentication (see Section 10). |
| `--mqvpn.address` | `127.0.0.1:9090` | mqvpn control API address (`host:port`). Must be reachable from the exporter process. |
| `--mqvpn.timeout` | `5s` | Per-RPC timeout when calling mqvpn. Each Prometheus scrape issues a fixed 4 RPCs (build_info cached 60s, get_stats, get_status, get_all_fec_stats) regardless of active-user count. |
| `--mqvpn.scrape-budget` | `10s` | Total time budget for one full scrape (all RPCs combined). Tuned to stay below your Prometheus `scrape_interval` so a slow scrape does not queue behind the next one. Each scrape is now a fixed 4 RPCs (build_info cached 60s, get_stats, get_status, get_all_fec_stats) regardless of active-user count, so the budget is far less likely to be exhausted than the v0.4-era per-user N+1 pattern. |
| `--metrics.include-endpoint` | `false` | If set, emit `mqvpn_client_info{user, endpoint}=1` so PromQL can detect endpoint changes (e.g. NAT rebinding). Off by default — mobile/CGNAT clients can churn endpoints frequently and inflate series cardinality. |

---

## 3. Prometheus Scrape Config

Paste into your `prometheus.yml` under `scrape_configs:`:

```yaml
scrape_configs:
  - job_name: mqvpn
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:9091']
```

A complete example file is at [`examples/prometheus.yml`](../examples/prometheus.yml).

**Recommended `scrape_interval`:** 15s matches mqvpn's default QUIC CC update
cadence. Lower intervals (< 5s) will cause concurrent RPCs and elevated
`mqvpn_exporter_scrape_failures_total` if mqvpn is under load.

---

## 4. Deployment Topology

The recommended deployment runs **everything on the VPN host** with all
services bound to loopback, and you reach the Grafana UI via SSH tunnel.
This matches the exporter's "no auth, loopback only" design (see §10) and
avoids running an authenticating reverse proxy.

### 4.1 Port plan

| Process | Bind | Notes |
|---------|------|-------|
| mqvpn control API | `127.0.0.1:9090` | Set by mqvpn `--control-port`. |
| `mqvpn-prometheus-exporter` | `127.0.0.1:9091` | Default. |
| Prometheus | `127.0.0.1:9092` | **Override the default** — Prometheus binds to `:9090` out of the box, which collides with mqvpn. Set `--web.listen-address=127.0.0.1:9092`. |
| Grafana | `127.0.0.1:3000` | Set `[server] http_addr = 127.0.0.1` in `grafana.ini`. |

### 4.2 Install with systemd

The exporter unit is at [`examples/systemd/mqvpn-exporter.service`](../examples/systemd/mqvpn-exporter.service).
Install Prometheus and Grafana from your distro packages on the same host
(Debian/Ubuntu: `apt install prometheus`; Grafana: official APT/YUM repo
per [grafana.com/docs/grafana/latest/setup-grafana/installation/](https://grafana.com/docs/grafana/latest/setup-grafana/installation/)).

**Prometheus** — drop the scrape config from
[`examples/prometheus.yml`](../examples/prometheus.yml) into
`/etc/prometheus/prometheus.yml`, then override the listen address with a
systemd drop-in:

```bash
sudo systemctl edit prometheus
```

```ini
[Service]
ExecStart=
ExecStart=/usr/bin/prometheus \
  --config.file=/etc/prometheus/prometheus.yml \
  --web.listen-address=127.0.0.1:9092 \
  --storage.tsdb.path=/var/lib/prometheus
```

The empty `ExecStart=` line clears the unit's default and is required —
without it systemd appends instead of replacing.

**Grafana** — edit `/etc/grafana/grafana.ini`:

```ini
[server]
http_addr = 127.0.0.1
http_port = 3000
```

Then enable everything:

```bash
sudo systemctl enable --now mqvpn-exporter prometheus grafana-server
```

Verify all three are loopback-only:

```bash
ss -tlnp | grep -E '127.0.0.1:(3000|9091|9092)'
```

If any of them shows `0.0.0.0:` or `[::]:`, a bind override didn't take —
fix it before continuing. Anything bound to a routable interface is
unauthenticated and must not stay that way.

### 4.3 Access Grafana via SSH tunnel

From your laptop:

```bash
ssh -L 3000:127.0.0.1:3000 vpn-host
```

Open `http://localhost:3000` in your browser (default credentials
`admin`/`admin`; change them on first login). The tunnel forwards your
laptop's port 3000 to Grafana on the VPN host's loopback. No public ports,
no reverse proxy, no certificate management.

For persistent access, replace the SSH tunnel with a Tailscale / WireGuard
/ mqvpn link to the host and visit `http://<private-ip>:3000` directly.
Same idea — Grafana is still loopback-bound, you've just brought your
client into a network where loopback is reachable.

### 4.4 Wire up the datasource

In Grafana → **Connections → Data sources → Add data source → Prometheus**,
set the URL to `http://127.0.0.1:9092` (the loopback Prometheus from §4.1).
Save & test, then proceed to §5 to import the bundled dashboard.

### 4.5 Compose stack (alternative)

[`examples/compose/`](../examples/compose/README.md) ships a Linux-only
`docker compose` stack that brings up the exporter (built from this
repo), Prometheus, and Grafana with `network_mode: host` (so they reach
mqvpn's host-loopback control API directly), pre-provisions the
datasource, auto-loads the bundled dashboard, and includes healthchecks.
mqvpn itself stays on the host — it's a VPN server with `CAP_NET_ADMIN`
/ TUN requirements and is out of scope for containerisation.

This is a peer alternative to the systemd setup in §4.2; both bind every
service to loopback. Pick compose for configuration-as-code, easy
host-to-host portability, and dependabot-driven image updates; pick
systemd if you don't want a Docker daemon on the host or prefer distro
packages with `unattended-upgrades`.

```bash
cd examples/compose
docker compose up -d --build
ssh -L 3000:127.0.0.1:3000 vpn-host
```

When iterating on exporter source, you can omit the exporter container
and run `go run .` on the host — see `examples/compose/README.md` for
that mode plus update / wipe / backup operations.

---

## 5. Grafana Dashboard

### Import the bundled dashboard

1. Open Grafana via the tunnel from §4.3 (`http://localhost:3000`).
2. Go to **Dashboards → Import**.
3. Upload `dashboards/mqvpn-grafana.json` from this repository.
4. Select your Prometheus datasource when prompted for `DS_PROMETHEUS`.
5. Click **Import**.

The dashboard uses three rows:
- **Row 1 — Server Overview** (always visible): connected clients, TX/RX throughput, server loss rate, uptime, active scheduler.
- **Row 2 — Per-User / Per-Path** (all schedulers): per-path TX, RX, SRTT, packet loss rate, paths-per-user stat.
- **Row 3 — Backup-FEC** (collapsed by default): FEC recovery ratio, standby-path byte ratio, FEC repair packets/sec, FEC negotiated indicator. Expand only when the `backup_fec` scheduler is active.

### Template variables

| Variable | Type | Description |
|----------|------|-------------|
| `$user` | multi-select (all) | Filters per-user and per-path panels to selected usernames. Derived from `label_values(mqvpn_client_paths, user)`. |
| `$scheduler` | single-select | Shows the current active scheduler. Derived from `label_values(mqvpn_build_info, scheduler)`. |

---

## 6. Metric Reference

All metrics use the `mqvpn_` namespace. Counters are monotonically increasing
and reset when mqvpn restarts. Use `rate()` for throughput; Prometheus handles
counter resets transparently.

### 6.1 Server-wide (no labels)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_server_clients` | Gauge | Number of currently connected clients. |
| `mqvpn_server_bytes_tx_total` | Counter | TUN bytes sent to all clients (server → clients). |
| `mqvpn_server_bytes_rx_total` | Counter | TUN bytes received from all clients (clients → server). |
| `mqvpn_server_dgram_sent_total` | Counter | QUIC datagrams sent across all sessions. |
| `mqvpn_server_dgram_recv_total` | Counter | QUIC datagrams received across all sessions. |
| `mqvpn_server_dgram_lost_total` | Counter | QUIC datagrams declared lost (server-observed). |
| `mqvpn_server_dgram_acked_total` | Counter | QUIC datagrams acknowledged. |
| `mqvpn_server_uptime_seconds` | Gauge | Seconds since `mqvpn_server_create()` was called. |
| `mqvpn_build_info` | Gauge (1) | Build metadata; value always 1. Labels: `version`, `scheduler`. |

### 6.2 Per-client (label: `user`)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_client_paths` | Gauge | Number of active paths for this client. |
| `mqvpn_client_bytes_tx_total` | Counter | TUN bytes sent to this client. |
| `mqvpn_client_bytes_rx_total` | Counter | TUN bytes received from this client. |
| `mqvpn_client_connected_seconds` | Gauge | Seconds since this client connected. |
| `mqvpn_client_info` | Gauge (1) | Per-client metadata (opt-in via `--metrics.include-endpoint`). Labels: `user`, `endpoint`. Value always 1. |

### 6.3 Per-path (labels: `user`, `path_id`)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_path_srtt_seconds` | Gauge | Smoothed RTT in seconds (srtt_ms / 1000). |
| `mqvpn_path_min_rtt_seconds` | Gauge | Minimum observed RTT in seconds. |
| `mqvpn_path_cwnd_bytes` | Gauge | Congestion window in bytes. |
| `mqvpn_path_in_flight_bytes` | Gauge | Bytes currently in flight on this path. |
| `mqvpn_path_bytes_tx_total` | Counter | Bytes sent on this path. |
| `mqvpn_path_bytes_rx_total` | Counter | Bytes received on this path. |
| `mqvpn_path_pkt_sent_total` | Counter | QUIC packets sent on this path. |
| `mqvpn_path_pkt_recv_total` | Counter | QUIC packets received on this path. |
| `mqvpn_path_pkt_lost_total` | Counter | QUIC packets declared lost on this path. |
| `mqvpn_path_state_info` | Gauge (1) | xquic transport path state as a label; value always 1. Extra label `state` is one of `init`, `validating`, `active`, `closing`, `closed`, `unknown` (see §7). |

### 6.4 Per-client FEC (label: `user`; requires `backup_fec` scheduler + FEC build)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_client_fec_enabled` | Gauge | 1 if FEC negotiated for this session, 0 otherwise. |
| `mqvpn_client_fec_send_total` | Counter | FEC repair packets sent. |
| `mqvpn_client_fec_recover_total` | Counter | Packets recovered by FEC decoder. |
| `mqvpn_client_lost_dgram_total` | Counter | QUIC datagrams reported lost for this client. **Per-session counter — resets when the client reconnects.** Not algebraically related to `mqvpn_server_dgram_lost_total` (which is process-wide cumulative). |
| `mqvpn_client_app_bytes_total` | Counter | Total application bytes carried (all paths). |
| `mqvpn_client_standby_app_bytes_total` | Counter | Application bytes delivered via standby path. |
| `mqvpn_client_mp_state_info` | Gauge (1) | xquic multipath state as a label; value always 1. Extra label `state` is one of `single_path`, `active_with_standby`, `standby_only`, `active_only`, `unknown` (see §7). |

### 6.5 Exporter self-stats (no labels)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_exporter_build_info` | Gauge (1) | Exporter version; label `version`. Value always 1. |
| `mqvpn_exporter_scrapes_total` | Counter | Total scrapes processed by this exporter (success + failure). Use as the denominator for SLI ratios with `mqvpn_exporter_scrape_failures_total`. |
| `mqvpn_exporter_scrape_failures_total` | Counter | Individual mqvpn RPC calls that failed during scrapes. A single scrape can contribute multiple failures. |
| `mqvpn_exporter_scrape_duration_seconds` | Histogram | Time to complete one full scrape of mqvpn. |

---

## 7. Enum Mappings

Both state metrics ship as **info-style** Prometheus metrics (value always 1,
state encoded as a label). Filter and group with PromQL `state="..."` rather
than numeric comparisons; mqvpn translates the underlying xquic enum to a
stable string per release, so the *label* values are the contract you can rely
on. The numeric `state` and `mp_state` fields in the JSON wire format are
preserved for legacy consumers but the labels here are derived from
`state_label` / `mp_state_label`, both added in mqvpn v0.5.0.

### 7.1 `mqvpn_path_state_info` — xquic transport path state

Maps to `xqc_path_state_t` (xquic `transport/xqc_multipath.h`). A path in
`active` is available for packet scheduling; other states are transient and
should resolve to `active` or `closed` within a few RTTs.

| Label | Meaning |
|-------|---------|
| `init` | Path is being initialised, not yet usable. |
| `validating` | Path validation (PATH_CHALLENGE / PATH_RESPONSE) in progress. |
| `active` | Path is fully validated and usable. |
| `closing` | Path is being gracefully closed (PATH_ABANDONED in flight). |
| `closed` | Path is closed (terminal). |
| `unknown` | xquic returned a state value mqvpn does not recognise (xquic enum was extended without an mqvpn label update). |

### 7.2 `mqvpn_client_mp_state_info` — xquic multipath session state

Computed by xquic from validated-path and standby-path counts (see
`xqc_conn_get_mp_stats`). Use this to detect degraded multipath rather than
inspecting individual `mqvpn_path_state_info` values.

| Label | Meaning |
|-------|---------|
| `single_path` | Single path or multipath disabled. |
| `active_with_standby` | Multiple validated paths, mix of available + standby — best, full redundancy. |
| `standby_only` | Only standby paths available — primary down, **degraded**. |
| `active_only` | Multiple paths, all available, no standby designated. |
| `unknown` | xquic returned a value mqvpn does not recognise. |

---

## 8. Counter Semantics

- **All `*_total` counters** start at 0 when `mqvpn_server_create()` is called
  and reset to 0 on server restart. The `bytes_tx`/`bytes_rx` client-level
  counters additionally reset when a client reconnects (new session).
- **Use `rate()` for throughput**, e.g. `rate(mqvpn_server_bytes_tx_total[1m])`.
  PromQL's `rate()` handles counter resets transparently.
- **FEC counters** (`fec_send_cnt`, `fec_recover_cnt`) are uint32 widened to
  uint64 on the wire; they wrap at ~4 billion events. Use `increase()` over
  short windows or `rate()` to avoid wrap artifacts.
- **`mqvpn_server_dgram_lost_total` vs `mqvpn_client_lost_dgram_total`** are
  **not algebraically related**. The server counter is a process-wide
  cumulative tracked outside any session and survives client disconnects;
  per-client counters are per-session and reset on disconnect. Do not write
  alerts that compare their sums or rates — they will diverge whenever a
  session closes, and the divergence is the expected behaviour.
- **Absent FEC metrics** — if the mqvpn server is built without
  `XQC_ENABLE_FEC`, or if the active scheduler is not `backup_fec`, the
  `mqvpn_client_fec_*`, `mqvpn_client_lost_dgram_total`,
  `mqvpn_client_app_bytes_total`, `mqvpn_client_standby_app_bytes_total`, and
  `mqvpn_client_mp_state_info` metrics will not appear at all in `/metrics`.
  This is expected — use `unless` or `or` in PromQL rules to tolerate absent
  series.

---

## 9. mqvpn Version Compatibility

| Exporter version | mqvpn version | Notes |
|-----------------|---------------|-------|
| 0.1.x | >= 0.5.0 | Requires `get_build_info`, `get_fec_stats` commands (new in v0.5.0). Older mqvpn is not supported — the first RPC of each scrape is `get_build_info`, and its failure aborts the scrape. |
| (future) 0.2.x | TBD | No breaking changes planned for the control API wire format. Additive fields tolerated. |

**Control API stability:** the `cmd`/`ok`/`error` envelope and all existing
field names within responses are stable across mqvpn minor and patch releases.
New optional fields may appear without a major version bump. Consumers MUST
ignore unknown JSON fields.

---

## 10. Safety

**Default: loopback only.** Both the exporter (`127.0.0.1:9091`) and the mqvpn
control API (`127.0.0.1:9090`) bind to loopback by default. Do not expose
either port to the network without an authenticating reverse proxy.

**This exporter has no authentication.** If you must expose metrics externally:

```nginx
server {
    listen 9091 ssl;
    auth_basic "mqvpn metrics";
    auth_basic_user_file /etc/nginx/.htpasswd;
    location /metrics {
        proxy_pass http://127.0.0.1:9091;
    }
}
```

**The exporter is a developer debugging tool.** It is not designed for
high-availability production observability. For production deployments:
- Run it on the same host as the mqvpn server (do not expose the control API
  across the network).
- Use the provided systemd unit (`examples/systemd/mqvpn-exporter.service`)
  which runs with `DynamicUser=yes`, `ProtectSystem=strict`, `PrivateTmp=yes`,
  and `NoNewPrivileges=yes`.
- Monitor `mqvpn_exporter_scrape_failures_total`; alert if it rises
  unexpectedly.

**Do NOT expose mqvpn's control API externally.** The control API has no
authentication and allows adding/removing users and reading all session
statistics.

---

## 11. Troubleshooting

### `/metrics` returns empty or only Go/process metrics

The exporter could not reach mqvpn. Check:
1. `mqvpn` is running with `--control-port 9090`.
2. The address matches `--mqvpn.address` (default `127.0.0.1:9090`).
3. Check exporter logs: `journalctl -u mqvpn-exporter -n 50`.
4. Test manually: `echo '{"cmd":"get_stats"}' | nc -q1 127.0.0.1 9090`.

### `mqvpn_exporter_scrape_failures_total` is rising

One or more RPC calls to mqvpn are failing per scrape. Common causes:
- **mqvpn restarted**: failures spike on restart, then recover automatically.
- **Scrape interval too low**: reduce `scrape_interval` to ≥ 15s, or increase `--mqvpn.timeout`.
- **mqvpn control connection limit hit** (max 8 concurrent): if other tools
  are calling the control API at the same time, the 9th connection receives an
  error. Increase `--mqvpn.timeout` to retry or reduce concurrent callers.

### FEC metrics absent from `/metrics`

This is expected in two scenarios:
1. **mqvpn built without `XQC_ENABLE_FEC`**: if `mqvpn_client_fec_enabled` does
   not appear at all in `/metrics` even with connected clients, FEC was not
   compiled in. (FEC presence is conveyed by the existence of the
   `mqvpn_client_fec_*` series, not by a label on `mqvpn_build_info` — that
   metric only carries `version` and `scheduler`.)
2. **Active scheduler is not `backup_fec`**: `get_all_fec_stats` returns
   `"fec not built"` if the scheduler does not populate FEC counters.

### Dashboard panels show "No data"

- Confirm Prometheus is scraping the exporter: check the Prometheus targets
  page at `http://localhost:9092/targets` (per the §4.1 port plan; if you
  kept Prometheus on its default `:9090`, use that — but note the collision
  with mqvpn's control API).
- Verify the `DS_PROMETHEUS` datasource is correctly configured in Grafana.
- FEC row panels will show "No data" if FEC metrics are absent (see above) —
  this is expected; keep the row collapsed when not using `backup_fec`.
- Path panels require at least one connected client. Connect a client to the
  mqvpn server and check again.
