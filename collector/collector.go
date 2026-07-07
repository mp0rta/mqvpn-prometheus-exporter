// Package collector implements the prometheus.Collector for mqvpn metrics.
//
// One Collect call performs five mqvpn JSON RPCs (constant, regardless of
// active-user count): get_build_info (cached 60s), get_stats (server-wide
// counters + uptime), get_reorder_stats (server-wide reorder buffer),
// get_status (per-client + per-path), and the bulk get_all_fec_stats
// (per-user FEC + multipath state). All metrics are
// produced as const metrics — no persistent gauges, no scrape-to-scrape
// state beyond the build_info cache and exporter self-stats.
package collector

import (
	"context"
	"errors"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/mp0rta/mqvpn-prometheus-exporter/client"
	"github.com/prometheus/client_golang/prometheus"
)

// Source abstracts the mqvpn client for testability.
type Source interface {
	GetBuildInfo(ctx context.Context) (*client.BuildInfoResponse, error)
	GetStats(ctx context.Context) (*client.StatsResponse, error)
	GetReorderStats(ctx context.Context) (*client.ReorderStatsResponse, error)
	GetStatus(ctx context.Context) (*client.StatusResponse, error)
	GetAllFECStats(ctx context.Context) (*client.AllFECStatsResponse, error)
}

// Config bundles the collector's tunables. New behaviour added in mqvpn v0.5.0:
//   - IncludeEndpoint: emit mqvpn_client_info{user, endpoint}=1. Off by default
//     because mobile/NAT clients can rebind endpoints frequently and inflate
//     series cardinality.
type Config struct {
	Source          Source
	Budget          time.Duration
	IncludeEndpoint bool
}

// Collector implements prometheus.Collector for mqvpn metrics. Each Collect
// call performs a fixed set of mqvpn RPCs and emits const metrics; the only
// in-process state is the build_info cache and the exporter self-stats.
type Collector struct {
	src             Source
	budget          time.Duration
	includeEndpoint bool

	// Build info is cached for 60s — version/scheduler don't change mid-run.
	buildMu sync.Mutex
	build   *client.BuildInfoResponse
	buildAt time.Time

	scrapesTotal   prometheus.Counter
	scrapeFailures prometheus.Counter
	scrapeDuration prometheus.Histogram
}

// DefaultScrapeBudget is the fallback overall scrape budget when a caller
// passes 0 to New. Sized to stay below a typical Prometheus scrape_interval
// (15s) so a slow mqvpn cannot queue scrapes against each other.
const DefaultScrapeBudget = 10 * time.Second

// New constructs a Collector from cfg. Budget <= 0 falls back to
// DefaultScrapeBudget. The returned Collector must be registered with a
// prometheus.Registerer before use.
func New(cfg Config) *Collector {
	budget := cfg.Budget
	if budget <= 0 {
		budget = DefaultScrapeBudget
	}
	return &Collector{
		src:             cfg.Source,
		budget:          budget,
		includeEndpoint: cfg.IncludeEndpoint,
		scrapesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mqvpn_exporter_scrapes_total",
			Help: "Total Prometheus scrapes processed by this exporter (success + failure).",
		}),
		scrapeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mqvpn_exporter_scrape_failures_total",
			Help: "Number of individual mqvpn RPC calls that failed during scrapes. A single scrape can contribute multiple failures.",
		}),
		scrapeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "mqvpn_exporter_scrape_duration_seconds",
			Help: "Time spent scraping mqvpn for one Prometheus scrape.",
		}),
	}
}

// Describe deliberately emits NOTHING — this is an "unchecked" collector per
// prometheus/client_golang's model. All metrics are produced via
// MustNewConstMetric in Collect with stable Descs. This avoids the bookkeeping
// of mirroring 20+ Descs in two places. scrapesTotal/scrapeFailures/
// scrapeDuration are auto-described when emitted via the Counter/Histogram
// interfaces.
func (c *Collector) Describe(_ chan<- *prometheus.Desc) {}

var (
	descServerClients    = prometheus.NewDesc("mqvpn_server_clients", "Number of currently-connected clients.", nil, nil)
	descServerBytesTx    = prometheus.NewDesc("mqvpn_server_bytes_tx_total", "Bytes the server has sent to clients.", nil, nil)
	descServerBytesRx    = prometheus.NewDesc("mqvpn_server_bytes_rx_total", "Bytes the server has received from clients.", nil, nil)
	descServerDgramSent  = prometheus.NewDesc("mqvpn_server_dgram_sent_total", "QUIC datagrams sent.", nil, nil)
	descServerDgramRecv  = prometheus.NewDesc("mqvpn_server_dgram_recv_total", "QUIC datagrams received.", nil, nil)
	descServerDgramLost  = prometheus.NewDesc("mqvpn_server_dgram_lost_total", "QUIC datagrams lost (server-observed, server-wide aggregate; not algebraically related to sum of mqvpn_client_lost_dgram_total).", nil, nil)
	descServerDgramAcked = prometheus.NewDesc("mqvpn_server_dgram_acked_total", "QUIC datagrams acknowledged.", nil, nil)
	descServerUptime     = prometheus.NewDesc("mqvpn_server_uptime_seconds", "Server uptime in seconds.", nil, nil)

	// Reorder buffer metrics — server-wide aggregate from get_reorder_stats.
	// Dedicated mqvpn_reorder_ namespace (not mqvpn_server_reorder_) because
	// reorder is a distinct subsystem with 17 metrics.
	descReorderDelivered          = prometheus.NewDesc("mqvpn_reorder_delivered_total", "Packets successfully delivered via reorder buffer.", nil, nil)
	descReorderTooLateDrop        = prometheus.NewDesc("mqvpn_reorder_too_late_drop_total", "Packets dropped as too late (sequence behind delivery window).", nil, nil)
	descReorderTooFarAheadDrop    = prometheus.NewDesc("mqvpn_reorder_too_far_ahead_drop_total", "Packets dropped as too far ahead of the expected sequence.", nil, nil)
	descReorderDuplicateDrop      = prometheus.NewDesc("mqvpn_reorder_duplicate_drop_total", "Duplicate packets dropped by reorder buffer.", nil, nil)
	descReorderPoolDrop           = prometheus.NewDesc("mqvpn_reorder_pool_drop_total", "Packets dropped due to global buffer pool exhaustion.", nil, nil)
	descReorderPerFlowLimitDrop   = prometheus.NewDesc("mqvpn_reorder_per_flow_limit_drop_total", "Packets dropped due to per-flow buffer cap.", nil, nil)
	descReorderResetDiscard       = prometheus.NewDesc("mqvpn_reorder_reset_discard_total", "Buffered packets discarded on flow reset.", nil, nil)
	descReorderGap                = prometheus.NewDesc("mqvpn_reorder_gap_total", "Gap episodes opened (buffer went empty to nonempty).", nil, nil)
	descReorderGapFilled          = prometheus.NewDesc("mqvpn_reorder_gap_filled_total", "Gap episodes closed by the missing sequence arriving.", nil, nil)
	descReorderGapTimeout         = prometheus.NewDesc("mqvpn_reorder_gap_timeout_total", "Gap episodes ended by timeout skip.", nil, nil)
	descReorderGapOverflow        = prometheus.NewDesc("mqvpn_reorder_gap_overflow_total", "Gap episodes ended by overflow flush.", nil, nil)
	descReorderGapDemote          = prometheus.NewDesc("mqvpn_reorder_gap_demote_total", "Gap episodes ended by ACK demote flush.", nil, nil)
	descReorderGapReset           = prometheus.NewDesc("mqvpn_reorder_gap_reset_total", "Gap episodes ended by flow reset discard.", nil, nil)
	descReorderAckDemote          = prometheus.NewDesc("mqvpn_reorder_ack_demote_total", "Flows demoted to pass-through by ACK classifier.", nil, nil)
	descReorderLatencyP99         = prometheus.NewDesc("mqvpn_reorder_added_latency_p99_seconds", "P99 added latency from reorder buffering.", nil, nil)
	descReorderLatencyMax         = prometheus.NewDesc("mqvpn_reorder_added_latency_max_seconds", "Maximum added latency from reorder buffering.", nil, nil)
	descReorderLatencyBufferedP99 = prometheus.NewDesc("mqvpn_reorder_added_latency_buffered_p99_seconds", "P99 added latency for packets that actually waited in the reorder buffer (excludes in-order pass-through).", nil, nil)

	// Hybrid mode (TCP lane) — server-wide from get_stats (mqvpn >= 0.9.0).
	// Client-only fields not decoded — see client.StatsResponse.
	descHybridFlowsActive   = prometheus.NewDesc("mqvpn_hybrid_tcp_flows_active", "Currently open egress TCP-lane flows (hybrid mode; whole-server). 0 when hybrid is disabled or mqvpn < 0.9.0.", nil, nil)
	descHybridFlowsTotal    = prometheus.NewDesc("mqvpn_hybrid_tcp_flows_total", "Cumulative egress TCP-lane flows opened since start (hybrid mode; whole-server; monotonic).", nil, nil)
	descHybridFlowsRejected = prometheus.NewDesc("mqvpn_hybrid_tcp_flows_rejected_total", "Cumulative egress TCP-lane flows rejected by a cap (hybrid mode; whole-server fd-budget + per-session TcpMaxFlows cap; ACL 403s and 5xx syscall failures are not caps and are not counted).", nil, nil)

	descBuildInfo = prometheus.NewDesc("mqvpn_build_info", "mqvpn build info; value is always 1.", []string{"version", "scheduler"}, nil)

	descClientPaths = prometheus.NewDesc("mqvpn_client_paths",
		"All path entries the server reports for this client, including closed/closing slots that xquic has not yet recycled. For active count use mqvpn_client_active_paths.",
		[]string{"user"}, nil)
	descClientActivePaths = prometheus.NewDesc("mqvpn_client_active_paths",
		"Paths in xquic state=active for this client. Excludes init/validating/closing/closed entries that mqvpn_client_paths still counts.",
		[]string{"user"}, nil)
	descClientBytesTx   = prometheus.NewDesc("mqvpn_client_bytes_tx_total", "Bytes sent to this client.", []string{"user"}, nil)
	descClientBytesRx   = prometheus.NewDesc("mqvpn_client_bytes_rx_total", "Bytes received from this client.", []string{"user"}, nil)
	descClientConnected = prometheus.NewDesc("mqvpn_client_connected_seconds", "Seconds since this client connected.", []string{"user"}, nil)
	descClientInfo      = prometheus.NewDesc("mqvpn_client_info",
		"Per-client metadata (opt-in via --metrics.include-endpoint); value is always 1.",
		[]string{"user", "endpoint"}, nil)

	descClientFECEnabled   = prometheus.NewDesc("mqvpn_client_fec_enabled", "1 if FEC is negotiated.", []string{"user"}, nil)
	descClientFECSend      = prometheus.NewDesc("mqvpn_client_fec_send_total", "FEC repair packets sent.", []string{"user"}, nil)
	descClientFECRecover   = prometheus.NewDesc("mqvpn_client_fec_recover_total", "Packets recovered by FEC decoder.", []string{"user"}, nil)
	descClientLostDgram    = prometheus.NewDesc("mqvpn_client_lost_dgram_total", "Datagrams reported lost for this client (per-session; resets on reconnect).", []string{"user"}, nil)
	descClientAppBytes     = prometheus.NewDesc("mqvpn_client_app_bytes_total", "Total application bytes carried.", []string{"user"}, nil)
	descClientStandbyBytes = prometheus.NewDesc("mqvpn_client_standby_app_bytes_total", "Application bytes via the standby path.", []string{"user"}, nil)
	descClientMPStateInfo  = prometheus.NewDesc("mqvpn_client_mp_state_info",
		"xquic multipath state as a label; value always 1. State is one of single_path, active_with_standby, standby_only, active_only, unknown.",
		[]string{"user", "state"}, nil)

	descPathSRTT      = prometheus.NewDesc("mqvpn_path_srtt_seconds", "Smoothed RTT.", []string{"user", "path_id"}, nil)
	descPathMinRTT    = prometheus.NewDesc("mqvpn_path_min_rtt_seconds", "Minimum observed RTT.", []string{"user", "path_id"}, nil)
	descPathCwnd      = prometheus.NewDesc("mqvpn_path_cwnd_bytes", "Congestion window.", []string{"user", "path_id"}, nil)
	descPathInFlight  = prometheus.NewDesc("mqvpn_path_in_flight_bytes", "Bytes in flight.", []string{"user", "path_id"}, nil)
	descPathBytesTx   = prometheus.NewDesc("mqvpn_path_bytes_tx_total", "Bytes sent on this path.", []string{"user", "path_id"}, nil)
	descPathBytesRx   = prometheus.NewDesc("mqvpn_path_bytes_rx_total", "Bytes received on this path.", []string{"user", "path_id"}, nil)
	descPathPktSent   = prometheus.NewDesc("mqvpn_path_pkt_sent_total", "Packets sent on this path.", []string{"user", "path_id"}, nil)
	descPathPktRecv   = prometheus.NewDesc("mqvpn_path_pkt_recv_total", "Packets received on this path.", []string{"user", "path_id"}, nil)
	descPathPktLost   = prometheus.NewDesc("mqvpn_path_pkt_lost_total", "Packets lost on this path.", []string{"user", "path_id"}, nil)
	descPathStateInfo = prometheus.NewDesc("mqvpn_path_state_info",
		"xquic per-path transport state as a label; value always 1. State is one of init, validating, active, closing, closed, unknown.",
		[]string{"user", "path_id", "state"}, nil)
)

// Collect performs one scrape against mqvpn (build_info / stats /
// reorder_stats / status / all_fec_stats) and emits the corresponding
// const metrics on ch. Failed
// RPCs increment scrapeFailures and log the cause; a partial scrape still
// emits whatever it could collect.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	t0 := time.Now()
	// Single deferred block — observe BEFORE emitting the histogram so the
	// value reflects THIS scrape (not the previous one). LIFO of two defers
	// would have observed AFTER emit and shipped a stale value.
	c.scrapesTotal.Inc()
	defer func() {
		c.scrapeDuration.Observe(time.Since(t0).Seconds())
		ch <- c.scrapesTotal
		ch <- c.scrapeFailures
		ch <- c.scrapeDuration
	}()

	ctx, cancel := context.WithTimeout(context.Background(), c.budget)
	defer cancel()

	bi, err := c.cachedBuildInfo(ctx)
	if err != nil {
		c.scrapeFailures.Inc()
		log.Printf("scrape: get_build_info: %v", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(descBuildInfo, prometheus.GaugeValue, 1,
		bi.Version, bi.Scheduler)

	// Server-wide stats (extended get_stats: dgram_*, uptime).
	serverStats, err := c.src.GetStats(ctx)
	if err != nil {
		c.scrapeFailures.Inc()
		log.Printf("scrape: get_stats: %v", err)
		// Continue — we can still report status/fec metrics below.
	} else {
		ch <- prometheus.MustNewConstMetric(descServerClients, prometheus.GaugeValue, float64(serverStats.NClients))
		ch <- prometheus.MustNewConstMetric(descServerBytesTx, prometheus.CounterValue, float64(serverStats.BytesTx))
		ch <- prometheus.MustNewConstMetric(descServerBytesRx, prometheus.CounterValue, float64(serverStats.BytesRx))
		ch <- prometheus.MustNewConstMetric(descServerDgramSent, prometheus.CounterValue, float64(serverStats.DgramSent))
		ch <- prometheus.MustNewConstMetric(descServerDgramRecv, prometheus.CounterValue, float64(serverStats.DgramRecv))
		ch <- prometheus.MustNewConstMetric(descServerDgramLost, prometheus.CounterValue, float64(serverStats.DgramLost))
		ch <- prometheus.MustNewConstMetric(descServerDgramAcked, prometheus.CounterValue, float64(serverStats.DgramAcked))
		ch <- prometheus.MustNewConstMetric(descServerUptime, prometheus.GaugeValue, float64(serverStats.UptimeSec))
		// Hybrid mode TCP-lane flow counters (additive get_stats fields; 0 below
		// mqvpn 0.9.0 or when hybrid is disabled).
		ch <- prometheus.MustNewConstMetric(descHybridFlowsActive, prometheus.GaugeValue, float64(serverStats.TCPFlowsActive))
		ch <- prometheus.MustNewConstMetric(descHybridFlowsTotal, prometheus.CounterValue, float64(serverStats.TCPFlowsTotal))
		ch <- prometheus.MustNewConstMetric(descHybridFlowsRejected, prometheus.CounterValue, float64(serverStats.TCPFlowsRejected))
	}

	// Reorder buffer stats — server-wide aggregate (mqvpn >= 0.8.0).
	if reorderResp, err := c.src.GetReorderStats(ctx); err == nil {
		rs := reorderResp.Reorder
		ch <- prometheus.MustNewConstMetric(descReorderDelivered, prometheus.CounterValue, float64(rs.DeliveredCount))
		ch <- prometheus.MustNewConstMetric(descReorderTooLateDrop, prometheus.CounterValue, float64(rs.TooLateDropCount))
		ch <- prometheus.MustNewConstMetric(descReorderTooFarAheadDrop, prometheus.CounterValue, float64(rs.TooFarAheadDropCount))
		ch <- prometheus.MustNewConstMetric(descReorderDuplicateDrop, prometheus.CounterValue, float64(rs.DuplicateDropCount))
		ch <- prometheus.MustNewConstMetric(descReorderPoolDrop, prometheus.CounterValue, float64(rs.PoolDropCount))
		ch <- prometheus.MustNewConstMetric(descReorderPerFlowLimitDrop, prometheus.CounterValue, float64(rs.PerFlowLimitDropCount))
		ch <- prometheus.MustNewConstMetric(descReorderResetDiscard, prometheus.CounterValue, float64(rs.ResetDiscardCount))
		ch <- prometheus.MustNewConstMetric(descReorderGap, prometheus.CounterValue, float64(rs.GapCount))
		ch <- prometheus.MustNewConstMetric(descReorderGapFilled, prometheus.CounterValue, float64(rs.GapFilledCount))
		ch <- prometheus.MustNewConstMetric(descReorderGapTimeout, prometheus.CounterValue, float64(rs.GapTimeoutCount))
		ch <- prometheus.MustNewConstMetric(descReorderGapOverflow, prometheus.CounterValue, float64(rs.GapOverflowCount))
		ch <- prometheus.MustNewConstMetric(descReorderGapDemote, prometheus.CounterValue, float64(rs.GapDemoteCount))
		ch <- prometheus.MustNewConstMetric(descReorderGapReset, prometheus.CounterValue, float64(rs.GapResetCount))
		ch <- prometheus.MustNewConstMetric(descReorderAckDemote, prometheus.CounterValue, float64(rs.AckDemoteCount))
		ch <- prometheus.MustNewConstMetric(descReorderLatencyP99, prometheus.GaugeValue, rs.AddedLatencyP99Ms/1000.0)
		ch <- prometheus.MustNewConstMetric(descReorderLatencyMax, prometheus.GaugeValue, rs.AddedLatencyMaxMs/1000.0)
		ch <- prometheus.MustNewConstMetric(descReorderLatencyBufferedP99, prometheus.GaugeValue, rs.AddedLatencyBufferedP99Ms/1000.0)
	} else if errors.Is(err, client.ErrReorderNotAvailable) {
		log.Printf("scrape: reorder stats not available (mqvpn < 0.8.0); omitting reorder metrics")
	} else {
		c.scrapeFailures.Inc()
		log.Printf("scrape: get_reorder_stats: %v", err)
	}

	st, err := c.src.GetStatus(ctx)
	if err != nil {
		c.scrapeFailures.Inc()
		log.Printf("scrape: get_status: %v", err)
		return
	}

	// Bulk FEC stats — one RPC for every active session, replacing the
	// previous per-user N+1 pattern. Build a username-keyed lookup so the
	// per-client emission below can mix per-status and per-fec data
	// without juggling parallel slices.
	fecByUser := map[string]*client.FECStatsEntry{}
	fecAvailable := false
	if afec, err := c.src.GetAllFECStats(ctx); err == nil {
		fecAvailable = true
		for i := range afec.Clients {
			e := &afec.Clients[i]
			fecByUser[e.User] = e
		}
	} else if errors.Is(err, client.ErrFECNotBuilt) {
		// FEC not compiled in. Per-status metrics still emit below; FEC
		// metrics simply do not appear, which is the documented behaviour.
		log.Printf("scrape: fec not built in mqvpn; omitting fec metrics for this scrape")
	} else {
		c.scrapeFailures.Inc()
		log.Printf("scrape: get_all_fec_stats: %v", err)
	}

	for i := range st.Clients {
		ci := &st.Clients[i]
		ch <- prometheus.MustNewConstMetric(descClientPaths, prometheus.GaugeValue,
			float64(len(ci.Paths)), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientBytesTx, prometheus.CounterValue,
			float64(ci.BytesTx), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientBytesRx, prometheus.CounterValue,
			float64(ci.BytesRx), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientConnected, prometheus.GaugeValue,
			float64(ci.ConnectedSec), ci.User)
		// Skip emit if Endpoint is empty (older mqvpn that does not return it).
		// A persistent endpoint="" series would silently break NAT-rebinding
		// alerts that key on `changes(mqvpn_client_info{user="X"}[5m])`.
		if c.includeEndpoint && ci.Endpoint != "" {
			ch <- prometheus.MustNewConstMetric(descClientInfo, prometheus.GaugeValue, 1,
				ci.User, ci.Endpoint)
		}

		activePaths := 0
		for j := range ci.Paths {
			p := &ci.Paths[j]
			// mqvpn get_status returns a fixed-size paths array; unused
			// slots carry path_id=UINT64_MAX with all counters zero.
			// Emitting them produces duplicate-label-set collection errors
			// because multiple unused slots share the same sentinel id.
			if p.PathID == math.MaxUint64 {
				continue
			}
			pid := strconv.FormatUint(p.PathID, 10)
			ch <- prometheus.MustNewConstMetric(descPathSRTT, prometheus.GaugeValue,
				float64(p.SRTTMs)/1000.0, ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathMinRTT, prometheus.GaugeValue,
				float64(p.MinRTTMs)/1000.0, ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathCwnd, prometheus.GaugeValue,
				float64(p.Cwnd), ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathInFlight, prometheus.GaugeValue,
				float64(p.InFlight), ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathBytesTx, prometheus.CounterValue,
				float64(p.BytesTx), ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathBytesRx, prometheus.CounterValue,
				float64(p.BytesRx), ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathPktSent, prometheus.CounterValue,
				float64(p.PktSent), ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathPktRecv, prometheus.CounterValue,
				float64(p.PktRecv), ci.User, pid)
			ch <- prometheus.MustNewConstMetric(descPathPktLost, prometheus.CounterValue,
				float64(p.PktLost), ci.User, pid)
			// Fall back to the numeric state if state_label is absent (older
			// mqvpn that has not been bumped to v0.5.0). The mqvpn-prometheus-
			// exporter ≥ 0.1 documents v0.5.0 as the supported floor, so this
			// is defensive only — alerts can rely on the canonical labels.
			label := p.StateLabel
			if label == "" {
				label = "unknown"
			}
			if label == "active" {
				activePaths++
			}
			ch <- prometheus.MustNewConstMetric(descPathStateInfo, prometheus.GaugeValue, 1,
				ci.User, pid, label)
		}
		ch <- prometheus.MustNewConstMetric(descClientActivePaths, prometheus.GaugeValue,
			float64(activePaths), ci.User)

		if !fecAvailable {
			continue
		}
		fec, ok := fecByUser[ci.User]
		if !ok {
			// User was in get_status but not in get_all_fec_stats. This is
			// the "race / disconnected mid-scrape" path or (more likely) a
			// half-attached session.  Skip silently — Prometheus staleness
			// handles the gap.
			continue
		}
		ch <- prometheus.MustNewConstMetric(descClientFECEnabled, prometheus.GaugeValue,
			float64(fec.EnableFEC), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientFECSend, prometheus.CounterValue,
			float64(fec.FECSendCnt), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientFECRecover, prometheus.CounterValue,
			float64(fec.FECRecoverCnt), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientLostDgram, prometheus.CounterValue,
			float64(fec.LostDgramCnt), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientAppBytes, prometheus.CounterValue,
			float64(fec.TotalAppBytes), ci.User)
		ch <- prometheus.MustNewConstMetric(descClientStandbyBytes, prometheus.CounterValue,
			float64(fec.StandbyAppBytes), ci.User)
		mpLabel := fec.MPStateLabel
		if mpLabel == "" {
			mpLabel = "unknown"
		}
		ch <- prometheus.MustNewConstMetric(descClientMPStateInfo, prometheus.GaugeValue, 1,
			ci.User, mpLabel)
	}
}

func (c *Collector) cachedBuildInfo(ctx context.Context) (*client.BuildInfoResponse, error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()
	if c.build != nil && time.Since(c.buildAt) < 60*time.Second {
		return c.build, nil
	}
	b, err := c.src.GetBuildInfo(ctx)
	if err != nil {
		return nil, err
	}
	c.build, c.buildAt = b, time.Now()
	return b, nil
}
