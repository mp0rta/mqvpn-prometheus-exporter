package client

import (
	"context"
	"errors"
	"net"
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
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		if _, err := c.Read(buf); err != nil {
			t.Logf("mock read: %v", err)
			return
		}
		c.Write([]byte(resp))
	}()
	return l.Addr().String(), func() { l.Close() }
}

func TestGetBuildInfo_OK(t *testing.T) {
	addr, stop := startMock(t,
		`{"ok":true,"version":"0.4.0","scheduler":"backup_fec","fec_enabled":1}`)
	defer stop()

	c := New(addr, 2*time.Second)
	info, err := c.GetBuildInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.4.0" {
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

// Note: errors import is here for B1.5's tests (using errors.Is on sentinel
// errors). Keeping it in this initial commit avoids gratuitous churn.
var _ = errors.Is
