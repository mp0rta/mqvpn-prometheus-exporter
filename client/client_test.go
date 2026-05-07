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
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		if _, err := c.Read(buf); err != nil {
			return // drop the error; t may already be dead
		}
		c.Write([]byte(resp))
	}()
	return l.Addr().String(), func() {
		l.Close()
		mu.Lock()
		if accepted != nil {
			accepted.Close()
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
}

func TestGetFECStats_OK(t *testing.T) {
	resp := `{"ok":true,"user":"alice","enable_fec":1,"mp_state":1,
             "fec_send_cnt":142,"fec_recover_cnt":17,"lost_dgram_cnt":23,
             "total_app_bytes":9123456,"standby_app_bytes":421337}`
	addr, stop := startMock(t, resp)
	defer stop()
	c := New(addr, 2*time.Second)
	r, err := c.GetFECStats(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if r.FECSendCnt != 142 || r.FECRecoverCnt != 17 {
		t.Errorf("counters: %+v", r)
	}
}

func TestGetFECStats_UserNotFound(t *testing.T) {
	addr, stop := startMock(t, `{"ok":false,"error":"user not found"}`)
	defer stop()
	c := New(addr, 2*time.Second)
	_, err := c.GetFECStats(context.Background(), "ghost")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("got %v", err)
	}
}

func TestGetFECStats_FECNotBuilt(t *testing.T) {
	addr, stop := startMock(t, `{"ok":false,"error":"fec not built"}`)
	defer stop()
	c := New(addr, 2*time.Second)
	_, err := c.GetFECStats(context.Background(), "alice")
	if !errors.Is(err, ErrFECNotBuilt) {
		t.Errorf("got %v", err)
	}
}
