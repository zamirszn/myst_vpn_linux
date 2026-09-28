// Package ipc is the control protocol between the privileged daemon and its
// unprivileged clients (the CLI and the tray applet).
//
// Shape: one JSON request per connection over a unix socket, one JSON response
// back, then the connection closes. Deliberately request/response rather than a
// streaming protocol — every operation here is fast and idempotent, and a
// long-lived duplex protocol would be more surface for no benefit.
//
// The socket is the security boundary. The daemon runs as root and will act on
// anything that can reach it, so the socket lives in a root-owned 0700 directory
// and the group is the only thing that widens access.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultSocketPath is where the daemon listens.
const DefaultSocketPath = "/run/mystd/mystd.sock"

// Commands accepted by the daemon.
const (
	CmdConnect    = "connect"
	CmdDisconnect = "disconnect"
	CmdStatus     = "status"
	CmdPing       = "ping"
	CmdCountries  = "countries"
	CmdLocations  = "locations"
	CmdLogin      = "login"
	CmdLogout     = "logout"
)

// Target names where a tunnel should exit.
//
// A struct rather than three positional strings, because country, city and
// ip_type are all short lowercase words and every ordering of them compiles.
// The API's answer to a mixed-up filter is a 2332 "no available instance for
// filters", which reads like an outage rather than a caller's mistake.
//
// Every field is optional: an empty Country lets the API choose the fastest
// node, an empty City takes any city in the country, and an empty IPType means
// the API's default, residential.
type Target struct {
	Country string
	City    string
	IPType  string
}

// Request is a control message from a client.
type Request struct {
	Command string `json:"command"`
	Country string `json:"country,omitempty"`
	City    string `json:"city,omitempty"`
	IPType  string `json:"ip_type,omitempty"`

	// Credentials carries a token set for CmdLogin, as the JSON the API
	// returned.
	//
	// It travels inline rather than as a filename because the daemon runs as
	// root: a root process told to open a path chosen by an unprivileged client
	// is a symlink attack waiting to happen. The payload is a few kilobytes and
	// crosses a socket that is already the trust boundary.
	Credentials json.RawMessage `json:"credentials,omitempty"`
}

// LocationCity is a city a country can exit through, as sent to front ends.
type LocationCity struct {
	// Slug is what the connect call takes, e.g. "london". Lowercase and matched
	// case-sensitively.
	Slug string `json:"slug"`

	// Name is the English display name, e.g. "London".
	Name string `json:"name"`
}

// LocationCountry is a country and its cities, as sent to front ends.
//
// This is a trimmed projection of the API's record, which also carries
// fourteen translations, coordinates, plan lists and node counts. Dropping
// those takes the residential set from about a megabyte to tens of kilobytes:
// it crosses a unix socket on every front-end start, and none of it is
// something a picker can show.
type LocationCountry struct {
	// Code is the uppercase ISO code, matching the country list.
	Code string `json:"code"`

	// Name is the English display name, e.g. "United Kingdom".
	Name string `json:"name"`

	// Cities may be empty: the API serves at least one country with no cities
	// listed. A picker must offer the country on its own rather than dropping
	// it.
	Cities []LocationCity `json:"cities,omitempty"`
}

// State is the tunnel's lifecycle position.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateError        State = "error"

	// StateReconnecting is the daemon recovering from a tunnel it watched die,
	// as opposed to StateConnecting, which is a user asking for a tunnel. The
	// distinction is worth keeping: one means "wait a moment", the other means
	// "something went wrong and I am fixing it", and a UI should say so.
	StateReconnecting State = "reconnecting"
)

// Status is the daemon's view of the world, returned by every command so a
// caller can render state without a follow-up round trip.
type Status struct {
	State     State     `json:"state"`
	Since     time.Time `json:"since,omitempty"`
	ExitIP    string    `json:"exit_ip,omitempty"`
	Country   string    `json:"country,omitempty"`
	City      string    `json:"city,omitempty"`
	IPType    string    `json:"ip_type,omitempty"`
	Interface string    `json:"interface,omitempty"`

	// IPv6Routed reports whether the active config carries IPv6. False means
	// IPv6 traffic is bypassing the tunnel on an IPv6-capable host.
	IPv6Routed bool `json:"ipv6_routed"`

	// KillSwitch reports whether the leak-prevention rules are installed.
	KillSwitch bool `json:"kill_switch"`

	// WakeLock reports whether suspend is currently inhibited on the tunnel's
	// behalf. False while connected means the machine is free to sleep, which
	// drops the tunnel.
	WakeLock bool `json:"wake_lock"`

	// DeviceSlots is the last observed device-slot usage, if known.
	DeviceSlots int `json:"device_slots,omitempty"`

	// LastError carries the most recent failure, so `myst status` can explain
	// why a connection dropped without the user reading logs.
	LastError string `json:"last_error,omitempty"`
}

// Response is the daemon's reply.
type Response struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`

	// Countries carries the answer to CmdCountries. It rides alongside Status
	// rather than in a response type of its own so that a single response shape
	// stays valid for every command.
	Countries []string `json:"countries,omitempty"`

	// Locations carries the answer to CmdLocations.
	Locations []LocationCountry `json:"locations,omitempty"`
}

// Handler is what the daemon must implement to serve IPC.
type Handler interface {
	Connect(ctx context.Context, t Target) (*Status, error)
	Disconnect(ctx context.Context) (*Status, error)
	Status(ctx context.Context) (*Status, error)

	// Countries lists the country codes the account can connect to.
	//
	// It is served from here rather than fetched by each front end because the
	// list lives behind an authenticated endpoint and the token belongs to the
	// daemon. A desktop applet running as the logged-in user would otherwise
	// need its own copy of the credential, which is the split this project
	// exists to avoid.
	Countries(ctx context.Context) ([]string, error)

	// Locations lists every country and city the account can exit through.
	//
	// Served from here for the same reason as Countries — it needs the token,
	// and the token is the daemon's — and cached here as well, because the
	// residential set is about a megabyte and a picker that re-fetched it every
	// time it was opened would be both slow and rude to the API.
	//
	// ipType selects the dataset: "residential" (the default when empty) or
	// "hosting".
	Locations(ctx context.Context, ipType string) ([]LocationCountry, error)

	// Login replaces the stored credentials with the token set in credentials,
	// which is the JSON body of an /oauth/token response.
	//
	// The credential file is root-only under the daemon's state directory, so
	// an unprivileged front end cannot write it — and should not need to, since
	// not holding the credential is the point of this split. The front end does
	// the part that needs a person (the magic link arrives by email) and hands
	// the result to the process that owns the file.
	Login(ctx context.Context, credentials []byte) (*Status, error)

	// Logout deletes the stored credentials. Served from here for the same
	// reason: the file is not the caller's to remove.
	Logout(ctx context.Context) (*Status, error)
}

// Server listens on a unix socket and dispatches to a Handler.
type Server struct {
	Path    string
	Handler Handler

	// Group is the group given ownership of the socket, as a name or a numeric
	// GID. It may be left empty, but see applyGroup for why it usually should
	// not be.
	Group string

	ln net.Listener
}

// Listen creates the socket, replacing any stale one from a previous run.
func (s *Server) Listen() error {
	if s.Path == "" {
		s.Path = DefaultSocketPath
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	// A leftover socket file from a crashed daemon would make Listen fail with
	// "address already in use" forever. Removing it is safe here because a live
	// daemon would hold the listener and we are not yet bound.
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	ln, err := net.Listen("unix", s.Path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.Path, err)
	}
	if err := os.Chmod(s.Path, 0o660); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	if err := s.applyGroup(); err != nil {
		ln.Close()
		return err
	}
	s.ln = ln
	return nil
}

// applyGroup hands the socket to the configured group.
//
// The daemon runs as root, so a socket it creates belongs to root:root. Mode
// 0660 then means only root can open it — which would leave the unprivileged
// CLI and the tray applet unable to reach the one thing they exist to talk to.
//
// The mode is deliberately not widened to 0666 instead. Anything that can open
// this socket can bring a root process to bring a tunnel up and down, so access
// is granted by group membership rather than to every account on the machine.
func (s *Server) applyGroup() error {
	if s.Group == "" {
		return nil
	}
	gid, err := lookupGID(s.Group)
	if err != nil {
		return fmt.Errorf("socket group %q: %w", s.Group, err)
	}
	// -1 leaves the owner alone. Passing 0 here would be a request to reset the
	// owner to root as well, which is not what this means and which fails
	// outright for any caller that is not already root.
	if err := os.Chown(s.Path, -1, gid); err != nil {
		return fmt.Errorf("chown socket to group %q: %w", s.Group, err)
	}
	return nil
}

// lookupGID accepts either a numeric GID or a group name.
//
// Names resolve through os/user, which reads /etc/group. That covers a local
// system group, which is what this is for; a group that exists only in LDAP or
// SSSD would need the numeric form.
func lookupGID(group string) (int, error) {
	if n, err := strconv.Atoi(group); err == nil {
		return n, nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

// Serve accepts connections until the context is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}

	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handle(ctx, conn)
	}
}

// Close stops accepting.
func (s *Server) Close() error {
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Bound the exchange: a client that opens a connection and then stalls must
	// not pin a goroutine in a root process indefinitely.
	conn.SetDeadline(time.Now().Add(2 * time.Minute))

	req, err := decodeRequest(conn)
	if err != nil {
		encode(conn, Response{Error: err.Error()})
		return
	}

	// Commands can take a while (connect provisions a tunnel), so give the
	// handler its own budget rather than inheriting the socket deadline.
	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var (
		status    *Status
		countries []string
		locations []LocationCountry
		herr      error
	)
	switch req.Command {
	case CmdConnect:
		status, herr = s.Handler.Connect(callCtx, Target{
			Country: req.Country,
			City:    req.City,
			IPType:  req.IPType,
		})
	case CmdDisconnect:
		status, herr = s.Handler.Disconnect(callCtx)
	case CmdStatus, CmdPing:
		status, herr = s.Handler.Status(callCtx)
	case CmdCountries:
		countries, herr = s.Handler.Countries(callCtx)
	case CmdLocations:
		locations, herr = s.Handler.Locations(callCtx, req.IPType)
	case CmdLogin:
		status, herr = s.Handler.Login(callCtx, req.Credentials)
	case CmdLogout:
		status, herr = s.Handler.Logout(callCtx)
	default:
		herr = fmt.Errorf("unknown command %q", req.Command)
	}

	if herr != nil {
		// Return last-known status alongside the error so a client can still
		// render something useful when a command fails.
		encode(conn, Response{Error: herr.Error(), Status: status})
		return
	}
	encode(conn, Response{OK: true, Status: status, Countries: countries, Locations: locations})
}

func decodeRequest(r io.Reader) (Request, error) {
	var req Request
	// Cap the request. Unbounded, this is an easy denial of service against a
	// root process — but the ceiling has to clear a token set, which rides in
	// on a login: an access token is a JWT of a few kilobytes, so a few hundred
	// bytes would reject every login. 16 KiB is roomy for a token and still
	// four orders of magnitude short of anything worth worrying about.
	limited := bufio.NewReader(io.LimitReader(r, 16<<10))
	if err := json.NewDecoder(limited).Decode(&req); err != nil {
		return Request{}, fmt.Errorf("decode request: %w", err)
	}
	if req.Command == "" {
		return Request{}, errors.New("request has no command")
	}
	return req, nil
}

func encode(w io.Writer, resp Response) {
	json.NewEncoder(w).Encode(resp)
}
