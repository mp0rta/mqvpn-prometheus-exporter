package collector

import (
	"context"
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
	fec    map[string]*client.FECStatsResponse
	fecErr map[string]error
}

func (f *fakeClient) GetBuildInfo(ctx context.Context) (*client.BuildInfoResponse, error) {
	return f.build, nil
}

// Defensive zero-default: tests that don't care about server-wide stats can
// leave f.stats nil.
func (f *fakeClient) GetStats(ctx context.Context) (*client.StatsResponse, error) {
	if f.stats == nil {
		return &client.StatsResponse{}, nil
	}
	return f.stats, nil
}

func (f *fakeClient) GetStatus(ctx context.Context) (*client.StatusResponse, error) {
	return f.status, nil
}

func (f *fakeClient) GetFECStats(ctx context.Context, user string) (*client.FECStatsResponse, error) {
	if e, ok := f.fecErr[user]; ok {
		return nil, e
	}
	return f.fec[user], nil
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
			Clients: []client.ClientInfo{{
				User: "alice", ConnectedSec: 42, BytesTx: 1000, BytesRx: 2000,
				Paths: []client.PathStats{{
					PathID: 0, SRTTMs: 31, MinRTTMs: 18, Cwnd: 196608,
					BytesTx: 900, BytesRx: 1900, PktSent: 50, PktRecv: 49, PktLost: 1, State: 0,
				}},
			}},
		},
		fec: map[string]*client.FECStatsResponse{
			"alice": {EnableFEC: 1, FECSendCnt: 142, FECRecoverCnt: 17,
				LostDgramCnt: 23, TotalAppBytes: 9123456, StandbyAppBytes: 421337,
				MPState: 1},
		},
	}
	coll := New(fc, 0)
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
}

func TestCollect_FECNotBuilt_OmitsFECMetrics(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 0},
		status: &client.StatusResponse{
			NClients: 1,
			Clients:  []client.ClientInfo{{User: "alice", Paths: []client.PathStats{}}},
		},
		fecErr: map[string]error{"alice": client.ErrFECNotBuilt},
	}
	coll := New(fc, 0)
	reg := prometheus.NewRegistry()
	reg.MustRegister(coll)

	n, err := testutil.GatherAndCount(reg, "mqvpn_client_fec_send_total")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 fec metrics, got %d", n)
	}
}

func TestCollect_UserNotFound_SkipsThatUserOnly(t *testing.T) {
	fc := &fakeClient{
		build: &client.BuildInfoResponse{Version: "0.5.0", Scheduler: "wlb", FECEnabled: 1},
		status: &client.StatusResponse{
			NClients: 2,
			Clients: []client.ClientInfo{
				{User: "alice", Paths: nil},
				{User: "bob", Paths: nil},
			},
		},
		fec: map[string]*client.FECStatsResponse{
			"bob": {EnableFEC: 1, FECSendCnt: 5},
		},
		fecErr: map[string]error{"alice": client.ErrUserNotFound},
	}
	coll := New(fc, 0)
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
