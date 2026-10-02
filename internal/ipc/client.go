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
	resp, err := c.call(ctx, req)
	if resp == nil {
		return nil, err
	}
	return resp.Status, err
}

// call sends one request and returns the whole response, for commands whose
// answer is not a status.
func (c Client) call(ctx context.Context, req Request) (*Response, error) {
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
		// Return the response too: on failure the daemon still reports what it
		// knows, which is usually more useful than the error alone.
		return &resp, errors.New(msg)
	}
	return &resp, nil
}

// Connect brings the tunnel up.
func (c Client) Connect(ctx context.Context, t Target) (*Status, error) {
	return c.Call(ctx, Request{
		Command: CmdConnect,
		Country: t.Country,
		City:    t.City,
		IPType:  t.IPType,
	})
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

// Countries lists the country codes the daemon's account can connect to, in the
// canonical uppercase form the API uses.
//
// The list comes from the daemon rather than from the API directly because
// fetching it needs the access token, and the token deliberately belongs to the
// daemon. This is how a front end offers every country without holding a
// credential.
func (c Client) Countries(ctx context.Context) ([]string, error) {
	resp, err := c.call(ctx, Request{Command: CmdCountries})
	if err != nil {
		return nil, err
	}
	return resp.Countries, nil
}

// Locations lists every country and city the daemon's account can exit through.
//
// ipType selects the dataset: "residential" (the default when empty) or
// "hosting". The daemon caches both, so this is cheap to call repeatedly — a
// picker can ask on every open without re-fetching a megabyte.
func (c Client) Locations(ctx context.Context, ipType string) ([]LocationCountry, error) {
	resp, err := c.call(ctx, Request{Command: CmdLocations, IPType: ipType})
	if err != nil {
		return nil, err
	}
	return resp.Locations, nil
}

// Login hands a token set to the daemon, which owns the credential file.
//
// credentials is the JSON body of an /oauth/token response. This call is how an
// unprivileged front end logs in: it does the interactive half of the flow and
// leaves the secret with the process that has somewhere safe to put it.
func (c Client) Login(ctx context.Context, credentials []byte) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdLogin, Credentials: credentials})
}

// Logout tells the daemon to delete the stored credentials.
func (c Client) Logout(ctx context.Context) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdLogout})
}

// LoginStart asks the daemon to email a login link to the address.
//
// It is the first half of the magic-link login. The daemon keeps the PKCE
// verifier, so the second half, LoginFinish, has to go to the same daemon
// process — restarting mystd in between means asking for a new link.
func (c Client) LoginStart(ctx context.Context, email string) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdLoginStart, Email: email})
}

// LoginFinish redeems the link, or the code in it, that the person pasted from
// the email, and stores the credentials it yields.
func (c Client) LoginFinish(ctx context.Context, pasted string) (*Status, error) {
	return c.Call(ctx, Request{Command: CmdLoginFinish, Code: pasted})
}
