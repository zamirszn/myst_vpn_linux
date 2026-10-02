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
	City    string `json:"city,omitempty"`
	IPType  string `json:"ip_type,omitempty"`

	// Email and Code carry the two halves of the magic-link login.
	Email string `json:"email,omitempty"`
	Code  string `json:"code,omitempty"`
}

type response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`

	// Countries answers the "countries" command. It rides alongside Status so
	// one response shape stays valid for every command.
	Countries []string `json:"countries,omitempty"`

	// Locations answers the "locations" command, by the same arrangement.
	Locations []LocationCountry `json:"locations,omitempty"`
}

// LocationCity is a city the account can exit through.
type LocationCity struct {
	// Slug is what the connect request takes, e.g. "london".
	Slug string `json:"slug"`

	// Name is the English display name, e.g. "London".
	Name string `json:"name"`
}

// LocationCountry is a country and the cities within it.
type LocationCountry struct {
	// Code is the uppercase ISO code, matching what Countries returns.
	Code string `json:"code"`

	// Name is the English display name, e.g. "United Kingdom".
	Name string `json:"name"`

	// Cities is empty for a country the API lists without cities, which is
	// still a valid choice — the country on its own.
	Cities []LocationCity `json:"cities,omitempty"`
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

	// LoggedIn is a pointer so that a daemon which does not send the field is
	// told apart from one that says false. Absent means "unknown", and a front
	// end must then behave as it did before accounts existed rather than put a
	// login screen over a daemon that is merely older than the window — the
	// stale-daemon trap the README warns about, in a more alarming costume.
	LoggedIn *bool `json:"logged_in,omitempty"`

	// Account is the email the daemon's credentials were issued for. Empty for
	// credentials imported from a file, which carry no address.
	Account string `json:"account,omitempty"`
}

// SignedOut reports whether the daemon said, in so many words, that it holds no
// credentials. False for an unknown answer: only a definite "no" counts.
func (s Status) SignedOut() bool { return s.LoggedIn != nil && !*s.LoggedIn }

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
	resp, err := c.call(ctx, req)
	if resp == nil {
		return nil, err
	}
	return resp.Status, err
}

// call sends one request and returns the whole response, for commands whose
// answer is not a status.
func (c Client) call(ctx context.Context, req request) (*response, error) {
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
		return &resp, errors.New(msg)
	}
	return &resp, nil
}

// Status reports the daemon's current state.
func (c Client) Status(ctx context.Context) (*Status, error) {
	return c.Call(ctx, request{Command: "status"})
}

// Target names where a tunnel should exit. An empty field is a preference not
// expressed: no country lets the API choose the fastest node, no city takes any
// city in the country.
type Target struct {
	Country string
	City    string
	IPType  string
}

// Connect brings a tunnel up in the requested region.
func (c Client) Connect(ctx context.Context, t Target) (*Status, error) {
	return c.Call(ctx, request{
		Command: "connect",
		Country: t.Country,
		City:    t.City,
		IPType:  t.IPType,
	})
}

// Disconnect tears the tunnel down.
func (c Client) Disconnect(ctx context.Context) (*Status, error) {
	return c.Call(ctx, request{Command: "disconnect"})
}

// Countries lists the country codes the account can connect to, in the
// canonical uppercase form the API uses.
//
// Asked of the daemon rather than of the API directly: the list needs the
// access token, and the token belongs to the daemon. This is what lets a front
// end offer every country without holding a credential of its own.
func (c Client) Countries(ctx context.Context) ([]string, error) {
	resp, err := c.call(ctx, request{Command: "countries"})
	if err != nil {
		return nil, err
	}
	return resp.Countries, nil
}

// Locations lists the countries and cities the account can exit through.
//
// ipType selects the dataset: "residential" (the default when empty) or
// "hosting". They are genuinely different lists rather than one filtered view —
// 106 countries against 49 — which is why the picker offers them as separate
// groups rather than as a filter.
//
// The daemon caches both, so calling this whenever a picker opens costs
// nothing after the first time.
func (c Client) Locations(ctx context.Context, ipType string) ([]LocationCountry, error) {
	resp, err := c.call(ctx, request{Command: "locations", IPType: ipType})
	if err != nil {
		return nil, err
	}
	return resp.Locations, nil
}

// LoginStart asks the daemon to email a login link to the address.
//
// The daemon keeps the secret that makes the link redeemable, so the second
// half, LoginFinish, must reach the same daemon process. Nothing here holds a
// credential, before or after.
func (c Client) LoginStart(ctx context.Context, email string) (*Status, error) {
	return c.Call(ctx, request{Command: "login-start", Email: email})
}

// LoginFinish hands the daemon what the person pasted from the email — the
// whole link, or the code in it. The daemon does the parsing, so every front
// end accepts the same shapes.
func (c Client) LoginFinish(ctx context.Context, pasted string) (*Status, error) {
	return c.Call(ctx, request{Command: "login-finish", Code: pasted})
}

// Logout tells the daemon to close any tunnel and delete its credentials.
func (c Client) Logout(ctx context.Context) (*Status, error) {
	return c.Call(ctx, request{Command: "logout"})
}
