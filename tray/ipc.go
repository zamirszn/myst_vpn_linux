package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// The control protocol is defined in the parent module's internal/ipc, which a
// separate Go module cannot import. Promoting those types into a public package
// would widen the daemon's API surface for the sake of a tray icon, so the
// handful of fields this applet reads are restated here.
//
// The wire format is JSON and is pinned by the daemon's own tests. If the two
// ever drift the tray degrades rather than lies: an unrecognised "state" value
// renders as disconnected, never as connected.
const defaultSocketPath = "/run/mystd/mystd.sock"

// Tunnel lifecycle states, as reported by the daemon.
//
// A root-module test reads this file and fails if it is missing a state the
// daemon can report, so the two cannot drift apart unnoticed.
const (
	stateDisconnected = "disconnected"
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
	stateError        = "error"
)

type request struct {
	Command string `json:"command"`
	Country string `json:"country,omitempty"`
	IPType  string `json:"ip_type,omitempty"`
}

type response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *status `json:"status,omitempty"`
}

// status mirrors ipc.Status, restricted to what the applet displays.
type status struct {
	State      string    `json:"state"`
	Since      time.Time `json:"since,omitempty"`
	ExitIP     string    `json:"exit_ip,omitempty"`
	Country    string    `json:"country,omitempty"`
	City       string    `json:"city,omitempty"`
	IPType     string    `json:"ip_type,omitempty"`
	Interface  string    `json:"interface,omitempty"`
	IPv6Routed bool      `json:"ipv6_routed"`
	KillSwitch bool      `json:"kill_switch"`
	WakeLock   bool      `json:"wake_lock"`
	LastError  string    `json:"last_error,omitempty"`
}

type client struct{ path string }

func (c client) socketPath() string {
	if c.path == "" {
		return defaultSocketPath
	}
	return c.path
}

// call sends one request and returns the resulting status.
func (c client) call(ctx context.Context, req request) (*status, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath())
	if err != nil {
		return nil, fmt.Errorf("cannot reach the daemon: %w", err)
	}
	defer conn.Close()

	// Connect provisions a tunnel upstream, so this has to outlast the
	// daemon's own 90s handler budget rather than cut it short.
	conn.SetDeadline(time.Now().Add(2 * time.Minute))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read daemon response: %w", err)
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "the daemon reported a failure with no detail"
		}
		return resp.Status, errors.New(msg)
	}
	return resp.Status, nil
}

func (c client) status(ctx context.Context) (*status, error) {
	return c.call(ctx, request{Command: "status"})
}

func (c client) connect(ctx context.Context, country, ipType string) (*status, error) {
	return c.call(ctx, request{Command: "connect", Country: country, IPType: ipType})
}

func (c client) disconnect(ctx context.Context) (*status, error) {
	return c.call(ctx, request{Command: "disconnect"})
}
