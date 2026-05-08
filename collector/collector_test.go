package collector

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/mp0rta/mqvpn-prometheus-exporter/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeClient satisfies the collector's Source interface without TCP.
type fakeClient struct {
	build  *client.BuildInfoResponse
	stats  *client.StatsResponse
	status *client.StatusResponse
	allFEC *client.AllFECStatsResponse
	fecErr error
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
