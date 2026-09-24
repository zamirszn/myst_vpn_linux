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
	"path/filepath"
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
)

// Request is a control message from a client.
type Request struct {
	Command string `json:"command"`
	Country string `json:"country,omitempty"`
	IPType  string `json:"ip_type,omitempty"`
}

// State is the tunnel's lifecycle position.
type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateError        State = "error"
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
}

// Handler is what the daemon must implement to serve IPC.
type Handler interface {
	Connect(ctx context.Context, country, ipType string) (*Status, error)
	Disconnect(ctx context.Context) (*Status, error)
	Status(ctx context.Context) (*Status, error)
}

// Server listens on a unix socket and dispatches to a Handler.
type Server struct {
	Path    string
	Handler Handler

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
	s.ln = ln
	return nil
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
		status *Status
		herr   error
	)
	switch req.Command {
	case CmdConnect:
		status, herr = s.Handler.Connect(callCtx, req.Country, req.IPType)
	case CmdDisconnect:
		status, herr = s.Handler.Disconnect(callCtx)
	case CmdStatus, CmdPing:
		status, herr = s.Handler.Status(callCtx)
	default:
		herr = fmt.Errorf("unknown command %q", req.Command)
	}

	if herr != nil {
		// Return last-known status alongside the error so a client can still
		// render something useful when a command fails.
		encode(conn, Response{Error: herr.Error(), Status: status})
		return
	}
	encode(conn, Response{OK: true, Status: status})
}

func decodeRequest(r io.Reader) (Request, error) {
	var req Request
	// Cap the request: these are three fields, and an unbounded read from a
	// root process is an easy denial of service.
	limited := bufio.NewReader(io.LimitReader(r, 4096))
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
