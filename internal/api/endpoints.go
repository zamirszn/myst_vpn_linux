package api

import (
	"context"
	"fmt"
	"time"
)

// WireguardConnectRequest provisions a tunnel.
//
// PublicKey is the only required field — the API is built for the caller to
// generate the keypair and transmit only the public half. Optional fields
// narrow server selection.
type WireguardConnectRequest struct {
	PublicKey  string `json:"public_key"`
	OS         string `json:"os_type,omitempty"`
	IPType     string `json:"ip_type,omitempty"`
	Country    string `json:"country,omitempty"`
	City       string `json:"city,omitempty"`
	UserIntent string `json:"user_intent,omitempty"`
	DNS        string `json:"dns,omitempty"`
	Reset      bool   `json:"reset_connection,omitempty"`
	TargetIP   string `json:"target_ip,omitempty"`
}

// WireguardConnectResponse carries the tunnel config.
//
// WGConfig is a template containing a %private_key% placeholder. The private
// key is substituted locally; the server never receives or returns it.
type WireguardConnectResponse struct {
	ID            string `json:"id"`
	WGConfig      string `json:"wg_config"`
	Hash          string `json:"hash"`
	ExitIP        string `json:"exit_ip"`
	LimitExceeded bool   `json:"limit_exceeded"`
	IPType        string `json:"ip_type"`
	Country       string `json:"country"`
	City          string `json:"city"`
}

// Connect provisions a WireGuard tunnel. Consumes a device slot.
func (c *Client) Connect(ctx context.Context, req WireguardConnectRequest) (*WireguardConnectResponse, error) {
	if req.PublicKey == "" {
		return nil, fmt.Errorf("public_key is required")
	}
	if req.OS == "" {
		req.OS = "linux"
	}
	var out WireguardConnectResponse
	if err := c.do(ctx, "POST", "/connection/connect", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Disconnect releases the connection and its device slot.
func (c *Client) Disconnect(ctx context.Context) error {
	return c.do(ctx, "GET", "/connection/disconnect", nil, nil)
}

// ConnectionConfig is the read-only connection surface: which countries are
// available and which are promoted.
type ConnectionConfig struct {
	Countries    []string `json:"countries"`
	TopCountries []string `json:"top_countries"`
}

// Config returns available countries. Useful for a country picker.
func (c *Client) Config(ctx context.Context) (*ConnectionConfig, error) {
	var out ConnectionConfig
	if err := c.do(ctx, "GET", "/connection/config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Region is a regional relay the client connects through.
type Region struct {
	ID           string   `json:"id"`
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	TopCountries []string `json:"top_countries"`
}

// Regions returns the gateway relays.
func (c *Client) Regions(ctx context.Context) ([]Region, error) {
	var out struct {
		Regions []Region `json:"regions"`
	}
	if err := c.do(ctx, "GET", "/connection/config/regions", nil, &out); err != nil {
		return nil, err
	}
	return out.Regions, nil
}

// Device is a registered device slot.
type Device struct {
	ID   string `json:"id"`
	Name string `json:"device_name"`
}

// Devices lists the account's device slots.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out struct {
		Data []Device `json:"data"`
	}
	if err := c.do(ctx, "GET", "/auth/sessions", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Check validates the token, returning nil if it is accepted.
func (c *Client) Check(ctx context.Context) error {
	return c.do(ctx, "GET", "/auth/check", nil, nil)
}

// Subscription describes the account plan.
type Subscription struct {
	Active      bool      `json:"active"`
	PlanID      string    `json:"plan_id"`
	ActiveUntil time.Time `json:"active_until"`
	Paused      bool      `json:"paused"`
	Expired     bool      `json:"expired"`
}

// SubscriptionStatus returns the account plan.
func (c *Client) SubscriptionStatus(ctx context.Context) (*Subscription, error) {
	var out Subscription
	if err := c.do(ctx, "GET", "/subscription", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
