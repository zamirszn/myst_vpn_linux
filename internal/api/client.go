// Package api is a client for the Mysterium VPN 2.0 consumer API.
//
// Wire format is snake_case throughout. The official OpenAPI-generated Dart
// client models these fields in camelCase, but that is its own mapping layer —
// the bytes on the wire are snake_case, as reflected in the struct tags here.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the production consumer API. The web dashboard at
// app.mysteriumvpn.com serves the same API; api.mysteriumvpn.com is the
// canonical host named by the generated client.
const DefaultBaseURL = "https://api.mysteriumvpn.com/api/v1"

// Client talks to the consumer API with a bearer token.
type Client struct {
	BaseURL string
	Token   string

	// HTTP is the underlying client. A non-zero timeout is important: a hung
	// connect call otherwise stalls the daemon's state machine indefinitely.
	HTTP *http.Client
}

// New returns a Client with sane defaults.
func New(token string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is a structured error response from the API.
type APIError struct {
	Status  int
	Code    string
	Message string
	Detail  string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("api %d %s: %s", e.Status, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("api %d: %s", e.Status, e.Message)
	default:
		return fmt.Sprintf("api returned HTTP %d", e.Status)
	}
}

// IsNoDeviceSlots reports the account device-limit condition.
//
// This surfaces as HTTP 500 with a distinctive body, so it must be matched on
// the message rather than the status — treating it as a generic server fault
// produces an error the user cannot act on.
func (e *APIError) IsNoDeviceSlots() bool {
	return strings.Contains(e.Message, "no device slots") ||
		strings.Contains(e.Detail, "allowed_devices_limit")
}

// IsNoServerInRegion reports that the requested country has no node able to
// serve a tunnel right now.
//
// This arrives as an HTTP 500 like a genuine server fault, and the body is the
// only thing that distinguishes them:
//
//	failed to find available wireguard server: no available instance for
//	filters: {Country:us ...}
//
// The difference matters to the user. A 500 says "the server broke, try later";
// this says "that country is unserved at the moment, pick another one" — which
// is something they can act on immediately.
func (e *APIError) IsNoServerInRegion() bool {
	return strings.Contains(e.Detail, "no available instance for filters")
}

// IsUnauthorized reports an expired or rejected token, which the caller should
// resolve by refreshing rather than by surfacing to the user.
func (e *APIError) IsUnauthorized() bool { return e.Status == http.StatusUnauthorized }

// errorEnvelope mirrors the API's error body shape.
type errorEnvelope struct {
	Status int `json:"status"`
	Error  struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	} `json:"error"`
}

// do performs a request and decodes a JSON response into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Cap the read: these responses are small, and an unbounded read from a
	// misbehaving endpoint is an easy way to exhaust memory in a root daemon.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		var env errorEnvelope
		if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
			apiErr.Code = env.Error.Code
			apiErr.Message = env.Error.Message
			apiErr.Detail = env.Error.Detail
		} else {
			apiErr.Message = strings.TrimSpace(string(raw))
		}
		return apiErr
	}

	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}
