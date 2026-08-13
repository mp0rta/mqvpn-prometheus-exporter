package collector

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/mp0rta/mqvpn-prometheus-exporter/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeClient satisfies the collector's Source interface without TCP.
type fakeClient struct {
	build      *client.BuildInfoResponse
	stats      *client.StatsResponse
	reorder    *client.ReorderStatsResponse
	reorderErr error
	status     *client.StatusResponse
	allFEC     *client.AllFECStatsResponse
	fecErr     error
}

func (f *fakeClient) GetBuildInfo(_ context.Context) (*client.BuildInfoResponse, error) {
	return f.build, nil
}

// Defensive zero-default: tests that don't care about server-wide stats can
// leave f.stats nil.
func (f *fakeClient) GetStats(_ context.Context) (*client.StatsResponse, error) {
	if f.stats == nil {
		return &client.StatsResponse{}, nil
	}
	return f.stats, nil
}

func (f *fakeClient) GetReorderStats(_ context.Context) (*client.ReorderStatsResponse, error) {
	if f.reorderErr != nil {
		return nil, f.reorderErr
	}
	if f.reorder == nil {
		return &client.ReorderStatsResponse{}, nil
	}
	return f.reorder, nil
}

func (f *fakeClient) GetStatus(_ context.Context) (*client.StatusResponse, error) {
	return f.status, nil
}

func (f *fakeClient) GetAllFECStats(_ context.Context) (*client.AllFECStatsResponse, error) {
	if f.fecErr != nil {
		return nil, f.fecErr
	}
	return f.allFEC, nil
}

func TestCollect_HappyPath(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "backup_fec", FECEnabled: 1},
		stats: &client.StatsResponse{
			NClients: 1, BytesTx: 1000, BytesRx: 2000,
			DgramSent: 100, DgramRecv: 99, DgramLost: 1, DgramAcked: 98,
			UptimeSec: 3601,
		},
		status: &client.StatusResponse{
			NClients: 1,
			Clients: []client.Info{{
				User: "alice", Endpoint: "1.2.3.4:443",
				ConnectedSec: 42, BytesTx: 1000, BytesRx: 2000,
				Paths: []client.PathStats{{
					PathID: 0, SRTTMs: 31, MinRTTMs: 18, Cwnd: 196608,
					BytesTx: 900, BytesRx: 1900, PktSent: 50, PktRecv: 49, PktLost: 1,
					State: 2, StateLabel: "active",
				}},
			}},
		},
		allFEC: &client.AllFECStatsResponse{
			NClients: 1,
			Clients: []client.FECStatsEntry{{
				User: "alice", EnableFEC: 1, MPState: 1, MPStateLabel: "active_with_standby",
				FECSendCnt: 142, FECRecoverCnt: 17, LostDgramCnt: 23,
				TotalAppBytes: 9123456, StandbyAppBytes: 421337,
			}},
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_server_clients Number of currently-connected clients.
# TYPE mqvpn_server_clients gauge
mqvpn_server_clients 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "mqvpn_server_clients"); err != nil {
		t.Error(err)
	}

	expectedUptime := `
# HELP mqvpn_server_uptime_seconds Server uptime in seconds.
# TYPE mqvpn_server_uptime_seconds gauge
mqvpn_server_uptime_seconds 3601
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedUptime), "mqvpn_server_uptime_seconds"); err != nil {
		t.Error(err)
	}

	// info-style state metrics: value is 1, label carries the state name.
	expectedPathState := `
# HELP mqvpn_path_state_info xquic per-path transport state as a label; value always 1. State is one of init, validating, active, closing, closed, unknown.
# TYPE mqvpn_path_state_info gauge
mqvpn_path_state_info{path_id="0",state="active",user="alice"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedPathState), "mqvpn_path_state_info"); err != nil {
		t.Error(err)
	}

	expectedMP := `
# HELP mqvpn_client_mp_state_info xquic multipath state as a label; value always 1. State is one of single_path, active_with_standby, standby_only, active_only, unknown.
# TYPE mqvpn_client_mp_state_info gauge
mqvpn_client_mp_state_info{state="active_with_standby",user="alice"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedMP), "mqvpn_client_mp_state_info"); err != nil {
		t.Error(err)
	}
}

func TestCollect_FECNotBuilt_OmitsFECMetrics(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{
			NClients: 1,
			Clients:  []client.Info{{User: "alice", Paths: []client.PathStats{}}},
		},
		fecErr: client.ErrFECNotBuilt,
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	n, err := testutil.GatherAndCount(reg, "mqvpn_client_fec_send_total")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 fec metrics, got %d", n)
	}
	// mp_state_info also vanishes because it is sourced from the FEC bulk.
	n, err = testutil.GatherAndCount(reg, "mqvpn_client_mp_state_info")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 mp_state_info metrics when FEC is unavailable, got %d", n)
	}
}

func TestCollect_BulkRaceUserMissing_SkipsThatUserOnly(t *testing.T) {
	// Mirrors the v0.4 N+1 "user disconnected mid-scrape" path: get_status
	// returned both, but the bulk FEC response only knows about bob.
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 1},
		status: &client.StatusResponse{
			NClients: 2,
			Clients: []client.Info{
				{User: "alice", Paths: nil},
				{User: "bob", Paths: nil},
			},
		},
		allFEC: &client.AllFECStatsResponse{
			NClients: 1,
			Clients: []client.FECStatsEntry{
				{User: "bob", EnableFEC: 1, MPStateLabel: "single_path", FECSendCnt: 5},
			},
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	n, err := testutil.GatherAndCount(reg, "mqvpn_client_fec_send_total")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 fec_send_total sample (bob only), got %d", n)
	}
}

// Closed paths linger in xquic's paths_info[] until the slot is recycled,
// so mqvpn_client_paths counts them but mqvpn_client_active_paths must not.
// A regression here would make the active-paths gauge useless for alerting on
// path loss (it would still report N even after every path went down).
func TestCollect_ActivePaths_ExcludesClosedAndValidating(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{
			NClients: 1,
			Clients: []client.Info{{
				User: "alice",
				Paths: []client.PathStats{
					{PathID: 0, State: 2, StateLabel: "active"},
					{PathID: 1, State: 4, StateLabel: "closed"},
					{PathID: 2, State: 1, StateLabel: "validating"},
					{PathID: 3, State: 2, StateLabel: "active"},
				},
			}},
		},
		fecErr: client.ErrFECNotBuilt,
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_client_active_paths Paths in xquic state=active for this client. Excludes init/validating/closing/closed entries that mqvpn_client_paths still counts.
# TYPE mqvpn_client_active_paths gauge
mqvpn_client_active_paths{user="alice"} 2
# HELP mqvpn_client_paths All path entries the server reports for this client, including closed/closing slots that xquic has not yet recycled. For active count use mqvpn_client_active_paths.
# TYPE mqvpn_client_paths gauge
mqvpn_client_paths{user="alice"} 4
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_client_active_paths", "mqvpn_client_paths"); err != nil {
		t.Error(err)
	}
}

func TestCollect_IncludeEndpoint_OptIn(t *testing.T) {
	mkFC := func() *fakeClient {
		return &fakeClient{
			build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
			status: &client.StatusResponse{
				NClients: 1,
				Clients: []client.Info{{
					User: "alice", Endpoint: "1.2.3.4:443", Paths: nil,
				}},
			},
			fecErr: client.ErrFECNotBuilt,
		}
	}

	regOff := prometheus.NewRegistry()
	regOff.MustRegister(New(Config{Source: mkFC()}))
	if n, err := testutil.GatherAndCount(regOff, "mqvpn_client_info"); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("default off: expected 0 mqvpn_client_info, got %d", n)
	}

	regOn := prometheus.NewRegistry()
	regOn.MustRegister(New(Config{Source: mkFC(), IncludeEndpoint: true}))
	expectedInfo := `
# HELP mqvpn_client_info Per-client metadata (opt-in via --metrics.include-endpoint); value is always 1.
# TYPE mqvpn_client_info gauge
mqvpn_client_info{endpoint="1.2.3.4:443",user="alice"} 1
`
	if err := testutil.GatherAndCompare(regOn, strings.NewReader(expectedInfo), "mqvpn_client_info"); err != nil {
		t.Error(err)
	}
}

func TestCollect_ScrapesTotalIncrementsEachCall(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{NClients: 0},
		fecErr: client.ErrFECNotBuilt,
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	// Each gather triggers exactly one Collect, which increments scrapesTotal
	// before emitting it. We assert TWICE — at counts 1 and 2 — so a future
	// regression that moves the Inc() into a sync.Once or skips it on the
	// happy path would fail the second assertion. A single-gather assertion
	// would pass even in that broken state.
	expectedFirst := `
# HELP mqvpn_exporter_scrapes_total Total Prometheus scrapes processed by this exporter (success + failure).
# TYPE mqvpn_exporter_scrapes_total counter
mqvpn_exporter_scrapes_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedFirst), "mqvpn_exporter_scrapes_total"); err != nil {
		t.Error(err)
	}
	expectedSecond := `
# HELP mqvpn_exporter_scrapes_total Total Prometheus scrapes processed by this exporter (success + failure).
# TYPE mqvpn_exporter_scrapes_total counter
mqvpn_exporter_scrapes_total 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expectedSecond), "mqvpn_exporter_scrapes_total"); err != nil {
		t.Error(err)
	}
}

func TestCollect_PathStateLabel_EmptyFallsBackToUnknown(t *testing.T) {
	// Older mqvpn (or a server build that drops state_label) returns the
	// numeric state without a label. The exporter must still emit a
	// well-formed info-style metric with state="unknown" so PromQL queries
	// keying on the `state` label do not silently match an empty string.
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		fecErr: client.ErrFECNotBuilt,
		status: &client.StatusResponse{
			NClients: 1,
			Clients: []client.Info{{
				User: "alice",
				Paths: []client.PathStats{
					{PathID: 0, State: 2, StateLabel: ""},
				},
			}},
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_path_state_info xquic per-path transport state as a label; value always 1. State is one of init, validating, active, closing, closed, unknown.
# TYPE mqvpn_path_state_info gauge
mqvpn_path_state_info{path_id="0",state="unknown",user="alice"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "mqvpn_path_state_info"); err != nil {
		t.Error(err)
	}
}

func TestCollect_IncludeEndpoint_EmptyEndpointSkipsEmit(t *testing.T) {
	// Older mqvpn (or any code path that returns ClientInfo without an
	// endpoint) must not emit `mqvpn_client_info{user="alice",endpoint=""}`.
	// A persistent empty-string label would silently break NAT-rebinding
	// alerts that look for `changes()` on this series.
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		fecErr: client.ErrFECNotBuilt,
		status: &client.StatusResponse{
			NClients: 1,
			Clients:  []client.Info{{User: "alice", Endpoint: "", Paths: nil}},
		},
	}
	coll := New(Config{Source: fc, IncludeEndpoint: true})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	if n, err := testutil.GatherAndCount(reg, "mqvpn_client_info"); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("expected 0 mqvpn_client_info samples for empty endpoint, got %d", n)
	}
}

// Regression: mqvpn returns a fixed-size paths array with unused slots
// carrying path_id=UINT64_MAX (state="init", counters zero). When more than
// one slot is unused, naïvely emitting them yields duplicate-label-set
// collection errors that crash /metrics with HTTP 500. The collector must
// skip the sentinel slots.
func TestCollect_PathIDSentinelDoesNotDuplicate(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		fecErr: client.ErrFECNotBuilt,
		status: &client.StatusResponse{
			NClients: 1,
			Clients: []client.Info{{
				User: "alice",
				Paths: []client.PathStats{
					{PathID: 0, State: 2, StateLabel: "active"},
					{PathID: math.MaxUint64, State: 0, StateLabel: "init"},
					{PathID: math.MaxUint64, State: 0, StateLabel: "init"},
					{PathID: math.MaxUint64, State: 0, StateLabel: "init"},
				},
			}},
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	// The Gather pipeline is what /metrics drives. If the collector emits
	// duplicate (user, path_id) tuples it surfaces here as an error.
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("Gather returned duplicate-label error: %v", err)
	}

	// Active slot should still produce exactly one sample per path metric.
	if n, err := testutil.GatherAndCount(reg, "mqvpn_path_state_info"); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Errorf("expected 1 mqvpn_path_state_info sample (active path only), got %d", n)
	}
}

func TestCollect_ReorderStats_HappyPath(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.8.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{NClients: 0},
		fecErr: client.ErrFECNotBuilt,
		reorder: &client.ReorderStatsResponse{
			Reorder: client.ReorderStats{
				GapCount: 100, GapFilledCount: 80, GapTimeoutCount: 10,
				GapOverflowCount: 5, GapDemoteCount: 3, GapResetCount: 2,
				AckDemoteCount: 7, TooLateDropCount: 15, TooFarAheadDropCount: 4,
				DuplicateDropCount: 12, PoolDropCount: 1, PerFlowLimitDropCount: 0,
				ResetDiscardCount: 3, DeliveredCount: 50000,
				AddedLatencyP99Ms: 12.345, AddedLatencyMaxMs: 42.1,
				AddedLatencyBufferedP99Ms: 18.7,
			},
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_reorder_ack_demote_total Flows demoted to pass-through by ACK classifier.
# TYPE mqvpn_reorder_ack_demote_total counter
mqvpn_reorder_ack_demote_total 7
# HELP mqvpn_reorder_added_latency_buffered_p99_seconds P99 added latency for packets that actually waited in the reorder buffer (excludes in-order pass-through).
# TYPE mqvpn_reorder_added_latency_buffered_p99_seconds gauge
mqvpn_reorder_added_latency_buffered_p99_seconds 0.018699999999999998
# HELP mqvpn_reorder_added_latency_max_seconds Maximum added latency from reorder buffering.
# TYPE mqvpn_reorder_added_latency_max_seconds gauge
mqvpn_reorder_added_latency_max_seconds 0.0421
# HELP mqvpn_reorder_added_latency_p99_seconds P99 added latency from reorder buffering.
# TYPE mqvpn_reorder_added_latency_p99_seconds gauge
mqvpn_reorder_added_latency_p99_seconds 0.012345
# HELP mqvpn_reorder_delivered_total Packets successfully delivered via reorder buffer.
# TYPE mqvpn_reorder_delivered_total counter
mqvpn_reorder_delivered_total 50000
# HELP mqvpn_reorder_duplicate_drop_total Duplicate packets dropped by reorder buffer.
# TYPE mqvpn_reorder_duplicate_drop_total counter
mqvpn_reorder_duplicate_drop_total 12
# HELP mqvpn_reorder_gap_demote_total Gap episodes ended by ACK demote flush.
# TYPE mqvpn_reorder_gap_demote_total counter
mqvpn_reorder_gap_demote_total 3
# HELP mqvpn_reorder_gap_filled_total Gap episodes closed by the missing sequence arriving.
# TYPE mqvpn_reorder_gap_filled_total counter
mqvpn_reorder_gap_filled_total 80
# HELP mqvpn_reorder_gap_overflow_total Gap episodes ended by overflow flush.
# TYPE mqvpn_reorder_gap_overflow_total counter
mqvpn_reorder_gap_overflow_total 5
# HELP mqvpn_reorder_gap_reset_total Gap episodes ended by flow reset discard.
# TYPE mqvpn_reorder_gap_reset_total counter
mqvpn_reorder_gap_reset_total 2
# HELP mqvpn_reorder_gap_timeout_total Gap episodes ended by timeout skip.
# TYPE mqvpn_reorder_gap_timeout_total counter
mqvpn_reorder_gap_timeout_total 10
# HELP mqvpn_reorder_gap_total Gap episodes opened (buffer went empty to nonempty).
# TYPE mqvpn_reorder_gap_total counter
mqvpn_reorder_gap_total 100
# HELP mqvpn_reorder_per_flow_limit_drop_total Packets dropped due to per-flow buffer cap.
# TYPE mqvpn_reorder_per_flow_limit_drop_total counter
mqvpn_reorder_per_flow_limit_drop_total 0
# HELP mqvpn_reorder_pool_drop_total Packets dropped due to global buffer pool exhaustion.
# TYPE mqvpn_reorder_pool_drop_total counter
mqvpn_reorder_pool_drop_total 1
# HELP mqvpn_reorder_reset_discard_total Buffered packets discarded on flow reset.
# TYPE mqvpn_reorder_reset_discard_total counter
mqvpn_reorder_reset_discard_total 3
# HELP mqvpn_reorder_too_far_ahead_drop_total Packets dropped as too far ahead of the expected sequence.
# TYPE mqvpn_reorder_too_far_ahead_drop_total counter
mqvpn_reorder_too_far_ahead_drop_total 4
# HELP mqvpn_reorder_too_late_drop_total Packets dropped as too late (sequence behind delivery window).
# TYPE mqvpn_reorder_too_late_drop_total counter
mqvpn_reorder_too_late_drop_total 15
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_reorder_delivered_total", "mqvpn_reorder_too_late_drop_total",
		"mqvpn_reorder_too_far_ahead_drop_total", "mqvpn_reorder_duplicate_drop_total",
		"mqvpn_reorder_pool_drop_total", "mqvpn_reorder_per_flow_limit_drop_total",
		"mqvpn_reorder_reset_discard_total", "mqvpn_reorder_gap_total",
		"mqvpn_reorder_gap_filled_total", "mqvpn_reorder_gap_timeout_total",
		"mqvpn_reorder_gap_overflow_total", "mqvpn_reorder_gap_demote_total",
		"mqvpn_reorder_gap_reset_total", "mqvpn_reorder_ack_demote_total",
		"mqvpn_reorder_added_latency_p99_seconds", "mqvpn_reorder_added_latency_max_seconds",
		"mqvpn_reorder_added_latency_buffered_p99_seconds"); err != nil {
		t.Error(err)
	}
}

func TestCollect_ReorderStats_NotAvailable(t *testing.T) {
	fc := &fakeClient{
		build:      &client.BuildInfoResponse{Version: "0.7.0", Scheduler: "wlb", FECEnabled: 0},
		status:     &client.StatusResponse{NClients: 0},
		fecErr:     client.ErrFECNotBuilt,
		reorderErr: client.ErrReorderNotAvailable,
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	// scrapeFailures should NOT increment for sentinel error. Check first
	// to avoid double-Gather counter inflation.
	expected := `
# HELP mqvpn_exporter_scrape_failures_total Number of individual mqvpn RPC calls that failed during scrapes. A single scrape can contribute multiple failures.
# TYPE mqvpn_exporter_scrape_failures_total counter
mqvpn_exporter_scrape_failures_total 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_exporter_scrape_failures_total"); err != nil {
		t.Error(err)
	}

	n, err := testutil.GatherAndCount(reg, "mqvpn_reorder_delivered_total")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 reorder metrics when not available, got %d", n)
	}
}

func TestCollect_ReorderStats_GenericError(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.8.0", Scheduler: "wlb", FECEnabled: 1},
		status: &client.StatusResponse{NClients: 1, Clients: []client.Info{{User: "alice", Paths: nil}}},
		allFEC: &client.AllFECStatsResponse{
			NClients: 1,
			Clients: []client.FECStatsEntry{{
				User: "alice", EnableFEC: 1, MPStateLabel: "single_path",
				FECSendCnt: 5,
			}},
		},
		reorderErr: errors.New("connection refused"),
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	// scrapeFailures SHOULD increment for non-sentinel error. Check FIRST
	// (single Gather = single Collect = scrapeFailures incremented once).
	expected := `
# HELP mqvpn_exporter_scrape_failures_total Number of individual mqvpn RPC calls that failed during scrapes. A single scrape can contribute multiple failures.
# TYPE mqvpn_exporter_scrape_failures_total counter
mqvpn_exporter_scrape_failures_total 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_exporter_scrape_failures_total"); err != nil {
		t.Error(err)
	}

	// Reorder metrics absent (second Gather, scrapeFailures now 2, but we
	// only filter on reorder metrics here so the counter value is irrelevant).
	n, err := testutil.GatherAndCount(reg, "mqvpn_reorder_delivered_total")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 reorder metrics on generic error, got %d", n)
	}

	// Per-client metrics should still be emitted (get_status succeeded).
	n, err = testutil.GatherAndCount(reg, "mqvpn_client_paths")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 mqvpn_client_paths (alice) despite reorder error, got %d", n)
	}

	// FEC metrics should still be emitted (get_all_fec_stats succeeded).
	n, err = testutil.GatherAndCount(reg, "mqvpn_client_fec_send_total")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 mqvpn_client_fec_send_total (alice) despite reorder error, got %d", n)
	}
}

func TestCollect_ReorderStats_AllZero(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.8.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{NClients: 0},
		fecErr: client.ErrFECNotBuilt,
		reorder: &client.ReorderStatsResponse{
			Reorder: client.ReorderStats{},
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	// Zero-valued counters must still be emitted, not skipped.
	n, err := testutil.GatherAndCount(reg,
		"mqvpn_reorder_delivered_total", "mqvpn_reorder_too_late_drop_total",
		"mqvpn_reorder_too_far_ahead_drop_total", "mqvpn_reorder_duplicate_drop_total",
		"mqvpn_reorder_pool_drop_total", "mqvpn_reorder_per_flow_limit_drop_total",
		"mqvpn_reorder_reset_discard_total", "mqvpn_reorder_gap_total",
		"mqvpn_reorder_gap_filled_total", "mqvpn_reorder_gap_timeout_total",
		"mqvpn_reorder_gap_overflow_total", "mqvpn_reorder_gap_demote_total",
		"mqvpn_reorder_gap_reset_total", "mqvpn_reorder_ack_demote_total",
		"mqvpn_reorder_added_latency_p99_seconds", "mqvpn_reorder_added_latency_max_seconds",
		"mqvpn_reorder_added_latency_buffered_p99_seconds")
	if err != nil {
		t.Fatal(err)
	}
	if n != 17 {
		t.Errorf("expected 17 zero-valued reorder metrics, got %d", n)
	}
}

func TestCollect_HybridFlows_HappyPath(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.9.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{NClients: 0},
		fecErr: client.ErrFECNotBuilt,
		stats: &client.StatsResponse{
			NClients: 1, BytesTx: 1, BytesRx: 2,
			TCPFlowsActive: 3, TCPFlowsTotal: 42, TCPFlowsRejected: 5,
		},
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_hybrid_tcp_flows_active Currently open egress TCP-lane flows (hybrid mode; whole-server). 0 when hybrid is disabled or mqvpn < 0.9.0.
# TYPE mqvpn_hybrid_tcp_flows_active gauge
mqvpn_hybrid_tcp_flows_active 3
# HELP mqvpn_hybrid_tcp_flows_rejected_total Cumulative egress TCP-lane flows rejected by a cap (hybrid mode; whole-server fd-budget + per-session TcpMaxFlows cap; ACL 403s and 5xx syscall failures are not caps and are not counted).
# TYPE mqvpn_hybrid_tcp_flows_rejected_total counter
mqvpn_hybrid_tcp_flows_rejected_total 5
# HELP mqvpn_hybrid_tcp_flows_total Cumulative egress TCP-lane flows opened since start (hybrid mode; whole-server; monotonic).
# TYPE mqvpn_hybrid_tcp_flows_total counter
mqvpn_hybrid_tcp_flows_total 42
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_hybrid_tcp_flows_active", "mqvpn_hybrid_tcp_flows_total",
		"mqvpn_hybrid_tcp_flows_rejected_total"); err != nil {
		t.Error(err)
	}
}

// mqvpn < 0.9.0 (or hybrid disabled): fields decode to 0 and MUST still be
// emitted (not omitted), so dashboards show a real 0 rather than a gap.
func TestCollect_HybridFlows_OldSchemaEmitsZero(t *testing.T) {
	fc := &fakeClient{
		build:  &client.BuildInfoResponse{Version: "0.8.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{NClients: 0},
		fecErr: client.ErrFECNotBuilt,
		stats:  &client.StatsResponse{NClients: 0}, // hybrid fields zero-valued
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_hybrid_tcp_flows_active Currently open egress TCP-lane flows (hybrid mode; whole-server). 0 when hybrid is disabled or mqvpn < 0.9.0.
# TYPE mqvpn_hybrid_tcp_flows_active gauge
mqvpn_hybrid_tcp_flows_active 0
# HELP mqvpn_hybrid_tcp_flows_rejected_total Cumulative egress TCP-lane flows rejected by a cap (hybrid mode; whole-server fd-budget + per-session TcpMaxFlows cap; ACL 403s and 5xx syscall failures are not caps and are not counted).
# TYPE mqvpn_hybrid_tcp_flows_rejected_total counter
mqvpn_hybrid_tcp_flows_rejected_total 0
# HELP mqvpn_hybrid_tcp_flows_total Cumulative egress TCP-lane flows opened since start (hybrid mode; whole-server; monotonic).
# TYPE mqvpn_hybrid_tcp_flows_total counter
mqvpn_hybrid_tcp_flows_total 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_hybrid_tcp_flows_active", "mqvpn_hybrid_tcp_flows_total",
		"mqvpn_hybrid_tcp_flows_rejected_total"); err != nil {
		t.Error(err)
	}
}

func TestCollect_UDPOffloadReinject_HappyPath(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.16.0", Scheduler: "wlb", FECEnabled: 0},
		stats: &client.StatsResponse{
			NClients: 1, UDPTxSends: 6, UDPTxDatagrams: 60,
			UDPRxReceives: 7, UDPRxDatagrams: 63,
		},
		status: &client.StatusResponse{NClients: 1, Clients: []client.Info{{
			User: "alice",
			Paths: []client.PathStats{{
				PathID: 0, BytesTx: 900, StateLabel: "active",
				ReinjectTxBytes: 5,
			}},
		}}},
		fecErr: client.ErrFECNotBuilt,
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_path_reinject_tx_bytes_total Cumulative bytes speculatively duplicated onto this path by reinjection. 0 when reinjection is off or mqvpn < 0.15.0.
# TYPE mqvpn_path_reinject_tx_bytes_total counter
mqvpn_path_reinject_tx_bytes_total{path_id="0",user="alice"} 5
# HELP mqvpn_udp_datagrams_total Outer-UDP datagrams moved, by direction. rate(datagrams)/rate(syscalls) per direction is the achieved GSO batching (tx) / GRO coalescing (rx) factor; 1.0 = one syscall per datagram (offload disabled or ineffective).
# TYPE mqvpn_udp_datagrams_total counter
mqvpn_udp_datagrams_total{direction="rx"} 63
mqvpn_udp_datagrams_total{direction="tx"} 60
# HELP mqvpn_udp_syscalls_total Outer-UDP send/receive syscalls that moved at least one datagram, by direction. Reads 0 on mqvpn < 0.16.0.
# TYPE mqvpn_udp_syscalls_total counter
mqvpn_udp_syscalls_total{direction="rx"} 7
mqvpn_udp_syscalls_total{direction="tx"} 6
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_udp_syscalls_total", "mqvpn_udp_datagrams_total",
		"mqvpn_path_reinject_tx_bytes_total"); err != nil {
		t.Error(err)
	}
}

// Old mqvpn: UDP fields decode to 0 and MUST still be emitted; the per-path
// reinject metric emits 0 for every existing path (it is absent only when
// the path itself is absent). The fixture therefore carries one client with
// one real path — with no paths the per-path pin would pass vacuously.
func TestCollect_UDPOffloadReinject_OldSchemaEmitsZero(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.14.0", Scheduler: "wlb", FECEnabled: 0},
		stats: &client.StatsResponse{NClients: 1}, // udp fields zero-valued
		status: &client.StatusResponse{NClients: 1, Clients: []client.Info{{
			User:  "alice",
			Paths: []client.PathStats{{PathID: 0, StateLabel: "active"}},
		}}},
		fecErr: client.ErrFECNotBuilt,
	}
	coll := New(Config{Source: fc})
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	expected := `
# HELP mqvpn_path_reinject_tx_bytes_total Cumulative bytes speculatively duplicated onto this path by reinjection. 0 when reinjection is off or mqvpn < 0.15.0.
# TYPE mqvpn_path_reinject_tx_bytes_total counter
mqvpn_path_reinject_tx_bytes_total{path_id="0",user="alice"} 0
# HELP mqvpn_udp_datagrams_total Outer-UDP datagrams moved, by direction. rate(datagrams)/rate(syscalls) per direction is the achieved GSO batching (tx) / GRO coalescing (rx) factor; 1.0 = one syscall per datagram (offload disabled or ineffective).
# TYPE mqvpn_udp_datagrams_total counter
mqvpn_udp_datagrams_total{direction="rx"} 0
mqvpn_udp_datagrams_total{direction="tx"} 0
# HELP mqvpn_udp_syscalls_total Outer-UDP send/receive syscalls that moved at least one datagram, by direction. Reads 0 on mqvpn < 0.16.0.
# TYPE mqvpn_udp_syscalls_total counter
mqvpn_udp_syscalls_total{direction="rx"} 0
mqvpn_udp_syscalls_total{direction="tx"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"mqvpn_udp_syscalls_total", "mqvpn_udp_datagrams_total",
		"mqvpn_path_reinject_tx_bytes_total"); err != nil {
		t.Error(err)
	}
}
