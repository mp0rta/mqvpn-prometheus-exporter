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

// Client is a JSON-over-TCP roundtripper for the mqvpn control API.
// Safe for concurrent use; each call opens a fresh connection.
type Client struct {
	addr    string
	timeout time.Duration
}

// New returns a Client that talks to the mqvpn control API at addr.
// timeout applies to both connection establishment and the per-call
// read/write deadline.
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
	defer func() { _ = conn.Close() }()

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

// GetBuildInfo issues the get_build_info RPC. Callers typically cache the
// result for ~60s since version/scheduler don't change at runtime.
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
// `StateLabel` was added in mqvpn v0.5.0; older servers do not return it and
// the field decodes to the empty string.
type PathStats struct {
	PathID     uint64 `json:"path_id"`
	SRTTMs     uint64 `json:"srtt_ms"`
	MinRTTMs   uint64 `json:"min_rtt_ms"`
	Cwnd       uint64 `json:"cwnd"`
	InFlight   uint64 `json:"in_flight"`
	BytesTx    uint64 `json:"bytes_tx"`
	BytesRx    uint64 `json:"bytes_rx"`
	PktSent    uint64 `json:"pkt_sent"`
	PktRecv    uint64 `json:"pkt_recv"`
	PktLost    uint64 `json:"pkt_lost"`
	State      uint8  `json:"state"`
	StateLabel string `json:"state_label"`
}

// Info is one element of StatusResponse.Clients — a per-session snapshot
// of a connected mqvpn user's TUN-byte counters and active paths. mqvpn's
// JSON nomenclature calls these "clients"; the Go type name avoids
// stuttering as client.Client*.
type Info struct {
	User         string      `json:"user"`
	Endpoint     string      `json:"endpoint"`
	ConnectedSec uint64      `json:"connected_sec"`
	BytesTx      uint64      `json:"bytes_tx"`
	BytesRx      uint64      `json:"bytes_rx"`
	Paths        []PathStats `json:"paths"`
}

// StatusResponse — see mqvpn/docs/control-api.md §5 get_status.
type StatusResponse struct {
	baseResponse
	NClients int    `json:"n_clients"`
	Clients  []Info `json:"clients"`
}

// GetStatus issues the get_status RPC, returning the per-client and per-path
// snapshot for every active session.
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
// extended in mqvpn v0.5.0 with dgram_*/uptime_sec; old fields keep position.
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

// GetStats issues the get_stats RPC, returning server-wide counters
// (bytes_tx/rx, datagram counters, uptime).
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

// ErrFECNotBuilt is the sentinel for the documented "fec not built" failure
// of GetAllFECStats — surfaced so the collector can drop FEC metrics for
// the rest of the scrape rather than emitting a per-call error log line.
var ErrFECNotBuilt = errors.New("fec not built")

// FECStatsEntry is one element of AllFECStatsResponse.Clients — the canonical
// per-user FEC + multipath snapshot returned by mqvpn's bulk get_all_fec_stats.
type FECStatsEntry struct {
	User            string `json:"user"`
	EnableFEC       uint8  `json:"enable_fec"`
	MPState         uint8  `json:"mp_state"`
	MPStateLabel    string `json:"mp_state_label"`
	FECSendCnt      uint64 `json:"fec_send_cnt"`
	FECRecoverCnt   uint64 `json:"fec_recover_cnt"`
	LostDgramCnt    uint64 `json:"lost_dgram_cnt"`
	TotalAppBytes   uint64 `json:"total_app_bytes"`
	StandbyAppBytes uint64 `json:"standby_app_bytes"`
}

// AllFECStatsResponse — see mqvpn/docs/control-api.md §5.8 get_all_fec_stats.
// Bulk variant that collapses N+1 RPCs (one per user) into a single call.
// Field name parity with GetStatus: a connected user is a "client"; the
// `users` nomenclature is reserved for list_users (registered auth-table).
type AllFECStatsResponse struct {
	baseResponse
	NClients int             `json:"n_clients"`
	Clients  []FECStatsEntry `json:"clients"`
}

// GetAllFECStats returns the bulk FEC stats for every active session.
// Returns ErrFECNotBuilt for the documented "fec not built" failure so the
// collector can drop FEC metrics for the rest of the scrape; any other
// non-OK response surfaces as a generic error.
func (c *Client) GetAllFECStats(ctx context.Context) (*AllFECStatsResponse, error) {
	body, err := c.Call(ctx, []byte(`{"cmd":"get_all_fec_stats"}`))
	if err != nil {
		return nil, err
	}
	var r AllFECStatsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse get_all_fec_stats response: %w (body=%q)", err, body)
	}
	if !r.OK {
		if r.Error == "fec not built" {
			return nil, ErrFECNotBuilt
		}
		return nil, fmt.Errorf("server error: %s", r.Error)
	}
	return &r, nil
}
