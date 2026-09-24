// Package daemonclient talks to the mystd control socket.
//
// It is shared by every desktop front end — the tray applet and the GTK
// window — so that they cannot drift apart in what they send or how they read
// the reply. Both are pure front ends: they hold no credentials, open no
// tunnel, and have no privileges. Everything they do is a request over this
// socket, which the only root process, the daemon, answers.
package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// DefaultSocketPath is where the daemon listens unless told otherwise.
const DefaultSocketPath = "/run/mystd/mystd.sock"

// Tunnel lifecycle states, as reported by the daemon.
//
// The control protocol is defined in the parent module's internal/ipc, which a
// separate Go module cannot import. Promoting those types into a public package
// would widen the daemon's API surface for the sake of a desktop applet, so the
// handful of fields a front end reads are restated here.
//
// The wire format is JSON and is pinned by the daemon's own tests. If the two
// ever drift these degrade rather than lie: an unrecognised State renders as
// disconnected, never as connected. A root-module test reads this file and
// fails if it is missing a state the daemon can report, so the two cannot drift
// apart unnoticed.
const (
	StateDisconnected = "disconnected"
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateReconnecting = "reconnecting"
	StateError        = "error"
)

type request struct {
	Command string `json:"command"`
	Country string `json:"country,omitempty"`
	IPType  string `json:"ip_type,omitempty"`
}

type response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Status mirrors ipc.Status, restricted to what a front end displays.
type Status struct {
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

// Client sends requests to the daemon. The zero value talks to the default
// socket.
type Client struct{ Path string }

func (c Client) socketPath() string {
	if c.Path == "" {
		return DefaultSocketPath
	}
	return c.Path
}

// Call sends one request and returns the resulting status.
func (c Client) Call(ctx context.Context, req request) (*Status, error) {
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

// Status reports the daemon's current state.
func (c Client) Status(ctx context.Context) (*Status, error) {
	return c.Call(ctx, request{Command: "status"})
}

// Connect brings a tunnel up, optionally in a named country.
func (c Client) Connect(ctx context.Context, country, ipType string) (*Status, error) {
	return c.Call(ctx, request{Command: "connect", Country: country, IPType: ipType})
}

// Disconnect tears the tunnel down.
func (c Client) Disconnect(ctx context.Context) (*Status, error) {
	return c.Call(ctx, request{Command: "disconnect"})
}
