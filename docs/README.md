# mqvpn-prometheus-exporter — Operator Guide

Sidecar Prometheus exporter for the [mqvpn](https://github.com/mp0rta/mqvpn)
multipath QUIC VPN server. Polls mqvpn's JSON control API and exposes
`/metrics` in Prometheus exposition format.

---

## 1. Quickstart

```bash
go install github.com/mp0rta/mqvpn-prometheus-exporter@latest
mqvpn --mode server --control-port 9090 ...   # start mqvpn first
mqvpn-prometheus-exporter --mqvpn.address 127.0.0.1:9090 --web.listen-address 127.0.0.1:9091
```

Open `http://127.0.0.1:9091/metrics` to verify output.

---

## 2. CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--web.listen-address` | `127.0.0.1:9091` | Address on which to expose `/metrics`. Defaults to loopback. Set to `0.0.0.0:9091` to expose externally — you MUST front with nginx for authentication (see Section 9). |
| `--mqvpn.address` | `127.0.0.1:9090` | mqvpn control API address (`host:port`). Must be reachable from the exporter process. |
| `--mqvpn.timeout` | `5s` | Per-RPC timeout when calling mqvpn. Each Prometheus scrape makes up to `1 + N_clients` RPCs (build_info cached 60s, get_stats, get_status, get_fec_stats per client). |

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

## 4. Grafana

### Import the bundled dashboard

```bash
# Start Grafana (if not already running)
docker run -d -p 3000:3000 --name grafana grafana/grafana:latest
```

1. Open `http://localhost:3000` (default credentials: `admin`/`admin`).
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

## 5. Metric Reference

All metrics use the `mqvpn_` namespace. Counters are monotonically increasing
and reset when mqvpn restarts. Use `rate()` for throughput; Prometheus handles
counter resets transparently.

### 5.1 Server-wide (no labels)

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

### 5.2 Per-client (label: `user`)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_client_paths` | Gauge | Number of active paths for this client. |
| `mqvpn_client_bytes_tx_total` | Counter | TUN bytes sent to this client. |
| `mqvpn_client_bytes_rx_total` | Counter | TUN bytes received from this client. |
| `mqvpn_client_connected_seconds` | Gauge | Seconds since this client connected. |

### 5.3 Per-path (labels: `user`, `path_id`)

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
| `mqvpn_path_state` | Gauge | xquic transport path state (numeric; see Section 6). |

### 5.4 Per-client FEC (label: `user`; requires `backup_fec` scheduler + FEC build)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_client_fec_enabled` | Gauge | 1 if FEC negotiated for this session, 0 otherwise. |
| `mqvpn_client_fec_send_total` | Counter | FEC repair packets sent. |
| `mqvpn_client_fec_recover_total` | Counter | Packets recovered by FEC decoder. |
| `mqvpn_client_lost_dgram_total` | Counter | QUIC datagrams reported lost for this client. |
| `mqvpn_client_app_bytes_total` | Counter | Total application bytes carried (all paths). |
| `mqvpn_client_standby_app_bytes_total` | Counter | Application bytes delivered via standby path. |
| `mqvpn_client_mp_state` | Gauge | xquic mp_state for this client (numeric; see Section 6). |

### 5.5 Exporter self-stats (no labels)

| Metric | Type | Description |
|--------|------|-------------|
| `mqvpn_exporter_build_info` | Gauge (1) | Exporter version; label `version`. Value always 1. |
| `mqvpn_exporter_scrape_failures_total` | Counter | Number of failed mqvpn RPC calls during scrapes. |
| `mqvpn_exporter_scrape_duration_seconds` | Histogram | Time to complete one full scrape of mqvpn. |

---

## 6. Enum Mappings

### 6.1 `mqvpn_path_state` — xquic transport path state

The `mqvpn_path_state` metric exposes the **raw xquic transport-layer path
state** (`xqc_path_state_t` in `xquic_typedef.h`), NOT the mqvpn scheduler's
logical role (primary / standby / etc.). Do not infer scheduler roles from
this value; the mapping is scheduler-specific and may change between mqvpn
versions.

Known values at xquic HEAD (for reference only; consult `xquic_typedef.h` for
the authoritative list):

| Value | Name | Meaning |
|-------|------|---------|
| 0 | `XQC_PATH_STATE_CREATING` | Path is being initialised, not yet usable. |
| 1 | `XQC_PATH_STATE_VALIDATING` | Path validation (PATH_CHALLENGE/RESPONSE) in progress. |
| 2 | `XQC_PATH_STATE_ACTIVE` | Path is fully validated and active. |
| 3 | `XQC_PATH_STATE_CLOSING` | Path is being gracefully closed. |
| 4 | `XQC_PATH_STATE_CLOSED` | Path is closed (terminal). |

A path in state 2 (`ACTIVE`) is available for packet scheduling. Other states
are transient and should resolve to ACTIVE or CLOSED within a few RTTs.

### 6.2 `mqvpn_client_mp_state` — xquic mp_state

The `mqvpn_client_mp_state` metric exposes the **xquic multipath session
state** (`xqc_multipath_state_t`). This is an internal xquic state machine
value. Known values:

| Value | Name | Meaning |
|-------|------|---------|
| 0 | `XQC_MULTIPATH_CREATED` | Multipath session just created, no paths yet. |
| 1 | `XQC_MULTIPATH_ACTIVE` | At least one validated path available. |
| 2 | `XQC_MULTIPATH_STANDBY` | Primary path lost; standby path in use. |
| 3 | `XQC_MULTIPATH_CLOSING` | Session closing, draining remaining paths. |

These values are diagnostic. Use `mqvpn_client_paths` to count available paths
rather than interpreting `mp_state` directly.

---

## 7. Counter Semantics

- **All `*_total` counters** start at 0 when `mqvpn_server_create()` is called
  and reset to 0 on server restart. The `bytes_tx`/`bytes_rx` client-level
  counters additionally reset when a client reconnects (new session).
- **Use `rate()` for throughput**, e.g. `rate(mqvpn_server_bytes_tx_total[1m])`.
  PromQL's `rate()` handles counter resets transparently.
- **FEC counters** (`fec_send_cnt`, `fec_recover_cnt`) are uint32 widened to
  uint64 on the wire; they wrap at ~4 billion events. Use `increase()` over
  short windows or `rate()` to avoid wrap artifacts.
- **Absent FEC metrics** — if the mqvpn server is built without
  `XQC_ENABLE_FEC`, or if the active scheduler is not `backup_fec`, the
  `mqvpn_client_fec_*`, `mqvpn_client_lost_dgram_total`,
  `mqvpn_client_app_bytes_total`, `mqvpn_client_standby_app_bytes_total`, and
  `mqvpn_client_mp_state` metrics will not appear at all in `/metrics`. This is
  expected — use `unless` or `or` in PromQL rules to tolerate absent series.

---

## 8. mqvpn Version Compatibility

| Exporter version | mqvpn version | Notes |
|-----------------|---------------|-------|
| 0.1.x | >= 0.4.0 | Requires `get_build_info`, `get_fec_stats` commands (new in v0.4.0). |
| 0.1.x | 0.3.x | Partial: `get_build_info` / `get_fec_stats` will fail; `mqvpn_exporter_scrape_failures_total` will be non-zero. Server-level metrics from `get_stats`/`get_status` still work. |
| (future) 0.2.x | TBD | No breaking changes planned for the control API wire format. Additive fields tolerated. |

**Control API stability:** the `cmd`/`ok`/`error` envelope and all existing
field names within responses are stable across mqvpn minor and patch releases.
New optional fields may appear without a major version bump. Consumers MUST
ignore unknown JSON fields.

---

## 9. Safety

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

## 10. Troubleshooting

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
1. **mqvpn built without `XQC_ENABLE_FEC`**: check `mqvpn_build_info` — if no
   `fec_enabled=1` label is present, FEC was not compiled in.
2. **Active scheduler is not `backup_fec`**: `get_fec_stats` returns
   `"fec not built"` if the scheduler does not populate FEC counters.

### Dashboard panels show "No data"

- Confirm Prometheus is scraping the exporter: check the Prometheus targets
  page at `http://localhost:9090/targets`.
- Verify the `DS_PROMETHEUS` datasource is correctly configured in Grafana.
- FEC row panels will show "No data" if FEC metrics are absent (see above) —
  this is expected; keep the row collapsed when not using `backup_fec`.
- Path panels require at least one connected client. Connect a client to the
  mqvpn server and check again.
