package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// startMock spawns a one-shot TCP server that returns `resp` to whatever
// request arrives on the next accepted connection. It mirrors mqvpn's
// control_socket pattern: read one chunk (small JSON request fits in one
// TCP segment), write the response, close. We do NOT io.ReadAll because the
// real client doesn't half-close — so ReadAll would deadlock waiting for EOF
// the client never sends.
func startMock(t *testing.T, resp string) (addr string, stop func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// We track the accepted connection so stop() can close it. The goroutine
	// must NOT call any *testing.T method (Log/Logf/Errorf/Fatal) — it can
	// outlive the test by up to the read deadline, and Go's testing package
	// panics if t.* is called after the test has finished.
	var (
		mu       sync.Mutex
		accepted net.Conn
	)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		mu.Lock()
		accepted = c
		mu.Unlock()
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		if _, err := c.Read(buf); err != nil {
			return // drop the error; t may already be dead
		}
		_, _ = c.Write([]byte(resp))
	}()
	return l.Addr().String(), func() {
		_ = l.Close()
		mu.Lock()
		if accepted != nil {
			_ = accepted.Close()
		}
		mu.Unlock()
	}
}

func TestGetBuildInfo_OK(t *testing.T) {
	addr, stop := startMock(t,
		`{"ok":true,"version":"0.5.0","scheduler":"backup_fec","fec_enabled":1}`)
	defer stop()

	c := New(addr, 2*time.Second)
	info, err := c.GetBuildInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.5.0" {
		t.Errorf("version: got %q", info.Version)
	}
	if info.Scheduler != "backup_fec" {
		t.Errorf("scheduler: got %q", info.Scheduler)
	}
	if info.FECEnabled != 1 {
		t.Errorf("fec_enabled: got %d", info.FECEnabled)
	}
}

func TestGetBuildInfo_ServerError(t *testing.T) {
	addr, stop := startMock(t, `{"ok":false,"error":"unknown cmd"}`)
	defer stop()
	c := New(addr, 2*time.Second)
	_, err := c.GetBuildInfo(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetStatus_TwoClients(t *testing.T) {
	resp := `{"ok":true,"n_clients":2,"clients":[
        {"user":"alice","endpoint":"1.2.3.4:443","connected_sec":42,
         "bytes_tx":1000,"bytes_rx":2000,
         "paths":[{"path_id":0,"srtt_ms":31,"min_rtt_ms":18,"cwnd":196608,
                   "in_flight":1024,"bytes_tx":900,"bytes_rx":1900,
                   "pkt_sent":50,"pkt_recv":49,"pkt_lost":1,"state":0}]},
        {"user":"bob","endpoint":"5.6.7.8:443","connected_sec":10,
         "bytes_tx":50,"bytes_rx":80,"paths":[]}]}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	s, err := c.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.NClients != 2 {
		t.Errorf("n_clients: %d", s.NClients)
	}
	if len(s.Clients) != 2 || s.Clients[0].User != "alice" {
		t.Errorf("clients: %+v", s.Clients)
	}
	if s.Clients[0].Paths[0].PathID != 0 || s.Clients[0].Paths[0].SRTTMs != 31 {
		t.Errorf("alice path[0]: %+v", s.Clients[0].Paths[0])
	}
}

func TestGetStats_OK(t *testing.T) {
	resp := `{"ok":true,"n_clients":2,"bytes_tx":12345,"bytes_rx":67890,
              "dgram_sent":111,"dgram_recv":110,"dgram_lost":1,"dgram_acked":109,
              "tcp_flows_active":3,"tcp_flows_total":42,"tcp_flows_rejected":5,
              "uptime_sec":3601}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	s, err := c.GetStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.NClients != 2 || s.BytesTx != 12345 || s.UptimeSec != 3601 ||
		s.DgramLost != 1 {
		t.Errorf("stats: %+v", s)
	}
	if s.TCPFlowsActive != 3 || s.TCPFlowsTotal != 42 || s.TCPFlowsRejected != 5 {
		t.Errorf("hybrid flows: %+v", s)
	}
}

// Old mqvpn (< 0.9.0) omits the hybrid keys entirely; they must decode to 0.
func TestGetStats_OldSchemaHybridZero(t *testing.T) {
	resp := `{"ok":true,"n_clients":1,"bytes_tx":1,"bytes_rx":2,
              "dgram_sent":0,"dgram_recv":0,"dgram_lost":0,"dgram_acked":0,
              "uptime_sec":10}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	s, err := c.GetStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.TCPFlowsActive != 0 || s.TCPFlowsTotal != 0 || s.TCPFlowsRejected != 0 {
		t.Errorf("expected absent hybrid fields to decode to 0, got %+v", s)
	}
}

func TestGetAllFECStats_OK(t *testing.T) {
	resp := `{"ok":true,"n_clients":2,"clients":[
        {"user":"alice","enable_fec":1,"mp_state":1,"mp_state_label":"active_with_standby",
         "fec_send_cnt":142,"fec_recover_cnt":17,"lost_dgram_cnt":23,
         "total_app_bytes":9123456,"standby_app_bytes":421337},
        {"user":"bob","enable_fec":1,"mp_state":0,"mp_state_label":"single_path",
         "fec_send_cnt":0,"fec_recover_cnt":0,"lost_dgram_cnt":0,
         "total_app_bytes":555555,"standby_app_bytes":0}]}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	r, err := c.GetAllFECStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.NClients != 2 || len(r.Clients) != 2 {
		t.Fatalf("n_clients=%d len=%d", r.NClients, len(r.Clients))
	}
	if r.Clients[0].User != "alice" || r.Clients[0].MPStateLabel != "active_with_standby" {
		t.Errorf("alice: %+v", r.Clients[0])
	}
	if r.Clients[1].User != "bob" || r.Clients[1].MPStateLabel != "single_path" {
		t.Errorf("bob: %+v", r.Clients[1])
	}
}

func TestGetAllFECStats_FECNotBuilt(t *testing.T) {
	addr, stop := startMock(t, `{"ok":false,"error":"fec not built"}`)
	defer stop()
	c := New(addr, 2*time.Second)
	_, err := c.GetAllFECStats(context.Background())
	if !errors.Is(err, ErrFECNotBuilt) {
		t.Errorf("got %v", err)
	}
}

func TestGetStatus_StateLabelDecoded(t *testing.T) {
	resp := `{"ok":true,"n_clients":1,"clients":[
        {"user":"alice","endpoint":"1.2.3.4:443","connected_sec":10,
         "bytes_tx":1,"bytes_rx":2,
         "paths":[{"path_id":0,"srtt_ms":5,"min_rtt_ms":3,"cwnd":1024,
                   "in_flight":0,"bytes_tx":1,"bytes_rx":2,
                   "pkt_sent":1,"pkt_recv":1,"pkt_lost":0,
                   "state":2,"state_label":"active"}]}]}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	s, err := c.GetStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Clients[0].Paths[0].StateLabel != "active" {
		t.Errorf("state_label: %q", s.Clients[0].Paths[0].StateLabel)
	}
}

func TestGetReorderStats_OK(t *testing.T) {
	resp := `{"ok":true,"reorder":{
		"gap_count":100,"gap_filled_count":80,"gap_timeout_count":10,
		"gap_overflow_count":5,"gap_demote_count":3,"gap_reset_count":2,
		"ack_demote_count":7,"too_late_drop_count":15,"too_far_ahead_drop_count":4,
		"duplicate_drop_count":12,"pool_drop_count":1,"per_flow_limit_drop_count":0,
		"reset_discard_count":3,"delivered_count":50000,
		"added_latency_p99_ms":12.345,"added_latency_max_ms":42.100,
		"added_latency_buffered_p99_ms":18.700}}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	r, err := c.GetReorderStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rs := r.Reorder
	if rs.GapCount != 100 {
		t.Errorf("gap_count: got %d", rs.GapCount)
	}
	if rs.GapFilledCount != 80 {
		t.Errorf("gap_filled_count: got %d", rs.GapFilledCount)
	}
	if rs.GapTimeoutCount != 10 {
		t.Errorf("gap_timeout_count: got %d", rs.GapTimeoutCount)
	}
	if rs.GapOverflowCount != 5 {
		t.Errorf("gap_overflow_count: got %d", rs.GapOverflowCount)
	}
	if rs.GapDemoteCount != 3 {
		t.Errorf("gap_demote_count: got %d", rs.GapDemoteCount)
	}
	if rs.GapResetCount != 2 {
		t.Errorf("gap_reset_count: got %d", rs.GapResetCount)
	}
	if rs.AckDemoteCount != 7 {
		t.Errorf("ack_demote_count: got %d", rs.AckDemoteCount)
	}
	if rs.TooLateDropCount != 15 {
		t.Errorf("too_late_drop_count: got %d", rs.TooLateDropCount)
	}
	if rs.TooFarAheadDropCount != 4 {
		t.Errorf("too_far_ahead_drop_count: got %d", rs.TooFarAheadDropCount)
	}
	if rs.DuplicateDropCount != 12 {
		t.Errorf("duplicate_drop_count: got %d", rs.DuplicateDropCount)
	}
	if rs.PoolDropCount != 1 {
		t.Errorf("pool_drop_count: got %d", rs.PoolDropCount)
	}
	if rs.PerFlowLimitDropCount != 0 {
		t.Errorf("per_flow_limit_drop_count: got %d", rs.PerFlowLimitDropCount)
	}
	if rs.ResetDiscardCount != 3 {
		t.Errorf("reset_discard_count: got %d", rs.ResetDiscardCount)
	}
	if rs.DeliveredCount != 50000 {
		t.Errorf("delivered_count: got %d", rs.DeliveredCount)
	}
	if rs.AddedLatencyP99Ms != 12.345 {
		t.Errorf("added_latency_p99_ms: got %f", rs.AddedLatencyP99Ms)
	}
	if rs.AddedLatencyMaxMs != 42.1 {
		t.Errorf("added_latency_max_ms: got %f", rs.AddedLatencyMaxMs)
	}
	if rs.AddedLatencyBufferedP99Ms != 18.7 {
		t.Errorf("added_latency_buffered_p99_ms: got %f", rs.AddedLatencyBufferedP99Ms)
	}
}

func TestGetReorderStats_UnknownCmd(t *testing.T) {
	addr, stop := startMock(t, `{"ok":false,"error":"unknown cmd"}`)
	defer stop()
	c := New(addr, 2*time.Second)
	_, err := c.GetReorderStats(context.Background())
	if !errors.Is(err, ErrReorderNotAvailable) {
		t.Errorf("expected ErrReorderNotAvailable, got %v", err)
	}
}

func TestGetReorderStats_ServerError(t *testing.T) {
	addr, stop := startMock(t, `{"ok":false,"error":"internal error"}`)
	defer stop()
	c := New(addr, 2*time.Second)
	_, err := c.GetReorderStats(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrReorderNotAvailable) {
		t.Error("should not be ErrReorderNotAvailable for internal error")
	}
}
