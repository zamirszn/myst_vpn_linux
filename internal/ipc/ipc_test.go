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
	lastTarget  Target

	countries    []string
	countriesErr error

	locations    []LocationCountry
	locationsErr error
	askedIPType  string

	credentials []byte
	loggedOut   bool
}

func (h *stubHandler) Connect(_ context.Context, t Target) (*Status, error) {
	h.lastTarget = t
	if h.connectErr != nil {
		return nil, h.connectErr
	}
	h.connected = true
	return &Status{State: StateConnected, Country: t.Country, City: t.City}, nil
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

func (h *stubHandler) Countries(context.Context) ([]string, error) {
	if h.countriesErr != nil {
		return nil, h.countriesErr
	}
	return h.countries, nil
}

func (h *stubHandler) Locations(_ context.Context, ipType string) ([]LocationCountry, error) {
	h.askedIPType = ipType
	if h.locationsErr != nil {
		return nil, h.locationsErr
	}
	return h.locations, nil
}

func (h *stubHandler) Login(_ context.Context, credentials []byte) (*Status, error) {
	h.credentials = credentials
	return &Status{State: StateDisconnected}, nil
}

func (h *stubHandler) Logout(context.Context) (*Status, error) {
	h.loggedOut = true
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

	st, err := c.Connect(ctx, Target{Country: "gb", IPType: "hosting"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if st.State != StateConnected {
		t.Errorf("state = %q, want %q", st.State, StateConnected)
	}
	if st.Country != "gb" {
		t.Errorf("country = %q, want gb", st.Country)
	}
	if h.lastTarget.Country != "gb" {
		t.Errorf("handler saw country %q, want gb", h.lastTarget.Country)
	}
	if h.lastTarget.IPType != "hosting" {
		t.Errorf("handler saw ip type %q, want hosting", h.lastTarget.IPType)
	}
}

func TestStatusAfterConnect(t *testing.T) {
	h := &stubHandler{}
	c := serve(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Connect(ctx, Target{Country: "de"}); err != nil {
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

	_, err := c.Connect(ctx, Target{Country: "gb"})
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

// TestLoginCarriesATokenSetIntact is aimed squarely at the request size cap.
//
// A real access token is a JWT of a few kilobytes. If the ceiling is set for
// the handful of short strings the other commands send, every login fails —
// and it fails by decoding a truncated request, so the error names a JSON
// syntax problem rather than a size limit, which is a miserable thing to
// debug. This sends a token of a realistic size and checks it arrives byte for
// byte.
func TestLoginCarriesATokenSetIntact(t *testing.T) {
	h := &stubHandler{}
	c := serve(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Roughly the shape and size of a real /oauth/token response.
	credentials := []byte(`{"access_token":"` + strings.Repeat("e", 3000) +
		`","refresh_token":"` + strings.Repeat("r", 200) +
		`","token_type":"Bearer","expires_in":2592000}`)
	if len(credentials) < 3000 {
		t.Fatal("the fixture is not big enough to prove anything")
	}

	if _, err := c.Login(ctx, credentials); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := string(h.credentials); got != string(credentials) {
		t.Errorf("the token set arrived changed: %d bytes in, %d bytes out", len(credentials), len(got))
	}
}

func TestLogoutReachesTheHandler(t *testing.T) {
	h := &stubHandler{}
	c := serve(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if !h.loggedOut {
		t.Error("the handler never saw the logout")
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

// TestDesktopFrontEndsKnowEveryState guards the one duplication the module split
// forces.
//
// The desktop front ends are a separate Go module and cannot import this
// package, so they restate the state vocabulary in daemonclient as plain
// strings. Nothing in the compiler connects the two: add a state here, forget
// it there, and every front end renders it as disconnected — a client that
// looks idle while the tunnel is in fact reconnecting or broken.
//
// The check is on the wire *values*, not the constant names, because the value
// is what actually crosses the socket.
func TestDesktopFrontEndsKnowEveryState(t *testing.T) {
	const shared = "../../desktop/internal/daemonclient/client.go"

	raw, err := os.ReadFile(shared)
	if err != nil {
		t.Fatalf("read %s: %v", shared, err)
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
			t.Errorf("daemonclient does not know the %q state — every desktop front end would render it as disconnected", st)
		}
	}
}

func TestCountriesRoundTrip(t *testing.T) {
	c := serve(t, &stubHandler{countries: []string{"GB", "US", "DE"}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := c.Countries(ctx)
	if err != nil {
		t.Fatalf("Countries: %v", err)
	}
	want := []string{"GB", "US", "DE"}
	if len(got) != len(want) {
		t.Fatalf("Countries() returned %d codes, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Countries()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCountriesErrorSurfaces makes sure a daemon that cannot reach the API
// reports that, rather than answering with an empty list a caller would render
// as "this account has no countries".
func TestCountriesErrorSurfaces(t *testing.T) {
	c := serve(t, &stubHandler{countriesErr: errors.New("list countries: api 401: token expired")})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Countries(ctx)
	if err == nil {
		t.Fatal("expected the handler's error to surface")
	}
	if got := err.Error(); got != "list countries: api 401: token expired" {
		t.Errorf("error = %q, want the handler's message verbatim", got)
	}
}

// TestStatusIsUnaffectedByTheNewField guards the shared response shape: adding
// Countries to Response must not disturb the commands that only want a status.
func TestStatusIsUnaffectedByTheNewField(t *testing.T) {
	c := serve(t, &stubHandler{countries: []string{"GB"}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st == nil {
		t.Fatal("Status returned nothing")
	}
	if st.State != StateDisconnected {
		t.Errorf("State = %q, want %q", st.State, StateDisconnected)
	}
}
