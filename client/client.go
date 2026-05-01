// Package client is a JSON-over-TCP roundtripper for the mqvpn server's
// control API (see github.com/mp0rta/mqvpn/docs/control-api.md).
//
// Each Call opens a fresh TCP connection, sends the request, reads the
// response, and closes — mirroring mqvpn's control_socket "close after
// response" pattern.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

type Client struct {
	addr    string
	timeout time.Duration
}

func New(addr string, timeout time.Duration) *Client {
	return &Client{addr: addr, timeout: timeout}
}

// Call opens a TCP connection, writes the request bytes (a JSON object),
// reads the response until EOF, and closes. The mqvpn server brace-counts
// the request and writes the response immediately, so a trailing newline is
// not required.
func (c *Client) Call(ctx context.Context, req []byte) ([]byte, error) {
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("dial mqvpn control API at %s: %w", c.addr, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(c.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return bytes.TrimSpace(body), nil
}

type baseResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// BuildInfoResponse — see mqvpn/docs/control-api.md §5 get_build_info.
type BuildInfoResponse struct {
	baseResponse
	Version    string `json:"version"`
	Scheduler  string `json:"scheduler"`
	FECEnabled int    `json:"fec_enabled"`
}

func (c *Client) GetBuildInfo(ctx context.Context) (*BuildInfoResponse, error) {
	body, err := c.Call(ctx, []byte(`{"cmd":"get_build_info"}`))
	if err != nil {
		return nil, err
	}
	var r BuildInfoResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse get_build_info response: %w (body=%q)", err, body)
	}
	if !r.OK {
		return nil, fmt.Errorf("server error: %s", r.Error)
	}
	return &r, nil
}

// PathStats — see mqvpn/docs/control-api.md §5 get_status, paths array.
type PathStats struct {
	PathID   uint64 `json:"path_id"`
	SRTTMs   uint64 `json:"srtt_ms"`
	MinRTTMs uint64 `json:"min_rtt_ms"`
	Cwnd     uint64 `json:"cwnd"`
	InFlight uint64 `json:"in_flight"`
	BytesTx  uint64 `json:"bytes_tx"`
	BytesRx  uint64 `json:"bytes_rx"`
	PktSent  uint64 `json:"pkt_sent"`
	PktRecv  uint64 `json:"pkt_recv"`
	PktLost  uint64 `json:"pkt_lost"`
	State    uint8  `json:"state"`
}

type ClientInfo struct {
	User         string      `json:"user"`
	Endpoint     string      `json:"endpoint"`
	ConnectedSec uint64      `json:"connected_sec"`
	BytesTx      uint64      `json:"bytes_tx"`
	BytesRx      uint64      `json:"bytes_rx"`
	Paths        []PathStats `json:"paths"`
}

type StatusResponse struct {
	baseResponse
	NClients int          `json:"n_clients"`
	Clients  []ClientInfo `json:"clients"`
}

func (c *Client) GetStatus(ctx context.Context) (*StatusResponse, error) {
	body, err := c.Call(ctx, []byte(`{"cmd":"get_status"}`))
	if err != nil {
		return nil, err
	}
	var r StatusResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse get_status response: %w (body=%q)", err, body)
	}
	if !r.OK {
		return nil, fmt.Errorf("server error: %s", r.Error)
	}
	return &r, nil
}

// StatsResponse — see mqvpn/docs/control-api.md §5 get_stats. The schema was
// extended in mqvpn v0.4.0 with dgram_*/uptime_sec; old fields keep position.
type StatsResponse struct {
	baseResponse
	NClients   int    `json:"n_clients"`
	BytesTx    uint64 `json:"bytes_tx"`
	BytesRx    uint64 `json:"bytes_rx"`
	DgramSent  uint64 `json:"dgram_sent"`
	DgramRecv  uint64 `json:"dgram_recv"`
	DgramLost  uint64 `json:"dgram_lost"`
	DgramAcked uint64 `json:"dgram_acked"`
	UptimeSec  uint64 `json:"uptime_sec"`
}

func (c *Client) GetStats(ctx context.Context) (*StatsResponse, error) {
	body, err := c.Call(ctx, []byte(`{"cmd":"get_stats"}`))
	if err != nil {
		return nil, err
	}
	var r StatsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse get_stats response: %w (body=%q)", err, body)
	}
	if !r.OK {
		return nil, fmt.Errorf("server error: %s", r.Error)
	}
	return &r, nil
}

// ErrUserNotFound and ErrFECNotBuilt are sentinel errors returned by
// GetFECStats so the collector can distinguish per-user race conditions
// (disconnected mid-scrape) from server-wide build flag absence.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrFECNotBuilt  = errors.New("fec not built")
)

// FECStatsResponse — see mqvpn/docs/control-api.md §5 get_fec_stats.
type FECStatsResponse struct {
	baseResponse
	User            string `json:"user"`
	EnableFEC       uint8  `json:"enable_fec"`
	MPState         uint8  `json:"mp_state"`
	FECSendCnt      uint64 `json:"fec_send_cnt"`
	FECRecoverCnt   uint64 `json:"fec_recover_cnt"`
	LostDgramCnt    uint64 `json:"lost_dgram_cnt"`
	TotalAppBytes   uint64 `json:"total_app_bytes"`
	StandbyAppBytes uint64 `json:"standby_app_bytes"`
}

// GetFECStats returns ErrUserNotFound or ErrFECNotBuilt for the two known
// non-OK responses; the collector uses errors.Is to distinguish them.
func (c *Client) GetFECStats(ctx context.Context, user string) (*FECStatsResponse, error) {
	req := fmt.Sprintf(`{"cmd":"get_fec_stats","user":%q}`, user)
	body, err := c.Call(ctx, []byte(req))
	if err != nil {
		return nil, err
	}
	var r FECStatsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse get_fec_stats response: %w (body=%q)", err, body)
	}
	if !r.OK {
		switch r.Error {
		case "user not found":
			return nil, ErrUserNotFound
		case "fec not built":
			return nil, ErrFECNotBuilt
		default:
			return nil, fmt.Errorf("server error: %s", r.Error)
		}
	}
	return &r, nil
}
