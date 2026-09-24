package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Client talks to a running daemon.
type Client struct{ Path string }

func (c Client) socketPath() string {
	if c.Path == "" {
		return DefaultSocketPath
	}
	return c.Path
}

// Call sends one request and returns the resulting status.
func (c Client) Call(ctx context.Context, req Request) (*Status, error) {
	path := c.socketPath()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the daemon at %s: %w\nis mystd running?  try: systemctl status mystd", path, err)
	}
	defer conn.Close()

	// Generous, because connect provisions a tunnel upstream. The daemon applies
	// its own, tighter budget on the handler.
	conn.SetDeadline(time.Now().Add(2 * time.Minute))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read daemon response: %w", err)
	}
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "daemon reported failure with no detail"
		}
		// Return the status too: on failure the daemon still reports what it
		// knows, which is usually more useful than the error alone.
		return resp.Status, errors.New(msg)
	}
	return resp.Status, nil
}

// Connect brings the tunnel up.
func (c Client) Connect(ctx context.Context, country, ipType string) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdConnect, Country: country, IPType: ipType})
}

// Disconnect tears the tunnel down.
func (c Client) Disconnect(ctx context.Context) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdDisconnect})
}

// Status reports current state.
func (c Client) Status(ctx context.Context) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdStatus})
}

// Ping checks whether a daemon is reachable at all.
func (c Client) Ping(ctx context.Context) error {
	_, err := c.Call(ctx, Request{Command: CmdPing})
	return err
}
