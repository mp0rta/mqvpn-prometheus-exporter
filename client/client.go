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

// ErrUserNotFound and ErrFECNotBuilt are sentinel errors returned by
// GetFECStats so the collector can distinguish per-user race conditions
// (disconnected mid-scrape) from server-wide build flag absence.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrFECNotBuilt  = errors.New("fec not built")
)
