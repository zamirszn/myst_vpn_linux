package ipc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type stubHandler struct {
	connected   bool
	connectErr  error
	lastCountry string
}

func (h *stubHandler) Connect(_ context.Context, country, _ string) (*Status, error) {
	h.lastCountry = country
	if h.connectErr != nil {
		return nil, h.connectErr
	}
	h.connected = true
	return &Status{State: StateConnected, Country: country}, nil
}

func (h *stubHandler) Disconnect(context.Context) (*Status, error) {
	h.connected = false
	return &Status{State: StateDisconnected}, nil
}

func (h *stubHandler) Status(context.Context) (*Status, error) {
	if h.connected {
		return &Status{State: StateConnected}, nil
	}
	return &Status{State: StateDisconnected}, nil
}

// serve starts a server on a temporary socket and returns a client for it.
func serve(t *testing.T, h Handler) Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sock")

	srv := &Server{Path: path, Handler: h}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx)
	t.Cleanup(func() { srv.Close() })

	return Client{Path: path}
}

func TestConnectRoundTrip(t *testing.T) {
	h := &stubHandler{}
	c := serve(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := c.Connect(ctx, "gb", "hosting")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if st.State != StateConnected {
		t.Errorf("state = %q, want %q", st.State, StateConnected)
	}
	if st.Country != "gb" {
		t.Errorf("country = %q, want gb", st.Country)
	}
	if h.lastCountry != "gb" {
		t.Errorf("handler saw country %q, want gb", h.lastCountry)
	}
}

func TestStatusAfterConnect(t *testing.T) {
	h := &stubHandler{}
	c := serve(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Connect(ctx, "de", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateConnected {
		t.Errorf("state = %q, want %q", st.State, StateConnected)
	}

	if _, err := c.Disconnect(ctx); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	st, err = c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateDisconnected {
		t.Errorf("state = %q, want %q", st.State, StateDisconnected)
	}
}

func TestHandlerErrorReachesTheClient(t *testing.T) {
	h := &stubHandler{connectErr: errors.New("account has no device slots left")}
	c := serve(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Connect(ctx, "gb", "")
	if err == nil {
		t.Fatal("expected the handler's error to surface")
	}
	if got := err.Error(); got != "account has no device slots left" {
		t.Errorf("error = %q, want the handler's message verbatim", got)
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	c := serve(t, &stubHandler{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Call(ctx, Request{Command: "self-destruct"}); err == nil {
		t.Fatal("expected an unknown command to be rejected")
	}
}

func TestListenRefusesAnUnknownGroup(t *testing.T) {
	// Naming a group is an explicit request. Silently falling back to a
	// root-only socket would leave the operator staring at a daemon that looks
	// healthy while every client gets "permission denied".
	path := filepath.Join(t.TempDir(), "sock")
	srv := &Server{Path: path, Handler: &stubHandler{}, Group: "no-such-group-myst-test"}

	if err := srv.Listen(); err == nil {
		srv.Close()
		t.Fatal("expected Listen to fail for a group that does not exist")
	}
}

func TestListenAcceptsANumericGID(t *testing.T) {
	// Numeric form must work without touching the group database — that is the
	// escape hatch when the group lives somewhere os/user cannot see.
	//
	// The current primary GID rather than 0, so the test exercises a real chown
	// while still passing unprivileged: a process may hand a file it owns to any
	// group it is already a member of, and to no other.
	path := filepath.Join(t.TempDir(), "sock")
	srv := &Server{Path: path, Handler: &stubHandler{}, Group: strconv.Itoa(os.Getgid())}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen with the caller's own GID: %v", err)
	}
	defer srv.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := info.Sys().(*syscall.Stat_t).Gid; int(got) != os.Getgid() {
		t.Errorf("socket GID = %d, want %d", got, os.Getgid())
	}
}

func TestLookupGIDAcceptsNamesAndNumbers(t *testing.T) {
	if gid, err := lookupGID("0"); err != nil || gid != 0 {
		t.Errorf("lookupGID(\"0\") = %d, %v; want 0, nil", gid, err)
	}

	// root exists on every system that can run this daemon.
	gid, err := lookupGID("root")
	if err != nil {
		t.Fatalf("lookupGID(\"root\"): %v", err)
	}
	if gid != 0 {
		t.Errorf("lookupGID(\"root\") = %d, want 0", gid)
	}

	if _, err := lookupGID("no-such-group-myst-test"); err == nil {
		t.Error("expected an error for a group that does not exist")
	}
}

// TestTrayKnowsEveryState guards the one duplication the module split forces.
//
// The tray is a separate Go module and cannot import this package, so it
// restates the state vocabulary in tray/ipc.go as plain strings. Nothing in the
// compiler connects the two: add a state here, forget it there, and the tray
// renders it as disconnected — a client that looks idle while the tunnel is in
// fact reconnecting or broken.
//
// The check is on the wire *values*, not the constant names, because the value
// is what actually crosses the socket.
func TestTrayKnowsEveryState(t *testing.T) {
	const traySource = "../../tray/ipc.go"

	raw, err := os.ReadFile(traySource)
	if err != nil {
		t.Fatalf("read %s: %v", traySource, err)
	}

	states := []State{
		StateDisconnected,
		StateConnecting,
		StateConnected,
		StateReconnecting,
		StateError,
	}
	for _, st := range states {
		if !strings.Contains(string(raw), `"`+string(st)+`"`) {
			t.Errorf("tray/ipc.go does not know the %q state — the tray would render it as disconnected", st)
		}
	}
}
