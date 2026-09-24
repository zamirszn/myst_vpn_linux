package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/api"
	"github.com/zamirszn/mysterium-linux-client/internal/auth"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
	"github.com/zamirszn/mysterium-linux-client/internal/wg"
)

// testTemplate mirrors the shape the API actually returns: a stock wg-quick
// config with the private key left as a placeholder for the client to fill in.
const testTemplate = `[Interface]
PrivateKey = %private_key%
Address = 10.181.86.246/32
DNS = 1.1.1.1

[Peer]
PublicKey = BWIx1BlaS5iZ5C+eYlONhfL6H749GebmdcEPLaphJzY=
AllowedIPs = 0.0.0.0/0
Endpoint = 91.98.228.210:56666
PersistentKeepalive = 25
`

type fakeTunnel struct {
	up           bool
	upCalls      int
	downCalls    int
	upErr        error
	handshake    time.Time
	handshakeErr error
	exitIP       string
	exitErr      error
}

func (f *fakeTunnel) Up(context.Context, string) error {
	f.upCalls++
	if f.upErr != nil {
		return f.upErr
	}
	f.up = true
	return nil
}

func (f *fakeTunnel) Down(context.Context) error {
	f.downCalls++
	f.up = false
	return nil
}

func (f *fakeTunnel) IsUp(context.Context) bool { return f.up }

func (f *fakeTunnel) Handshake(context.Context) (time.Time, error) {
	return f.handshake, f.handshakeErr
}

func (f *fakeTunnel) ExitIP(context.Context) (string, error) { return f.exitIP, f.exitErr }

type fakeAPI struct {
	connectResp *api.WireguardConnectResponse
	connectErr  error
	disconnects int
	lastReq     api.WireguardConnectRequest
}

func (f *fakeAPI) Connect(_ context.Context, req api.WireguardConnectRequest) (*api.WireguardConnectResponse, error) {
	f.lastReq = req
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	return f.connectResp, nil
}

func (f *fakeAPI) Disconnect(context.Context) error {
	f.disconnects++
	return nil
}

type fakeTokens struct {
	token *auth.Token
	err   error
}

func (f *fakeTokens) EnsureValid(context.Context, auth.Store, time.Duration) (*auth.Token, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.token, nil
}

type fakeLock struct{ released bool }

func (l *fakeLock) Release() { l.released = true }

// newTestDaemon builds a daemon wired entirely to fakes. tune may adjust any
// piece before construction.
func newTestDaemon(t *testing.T, tune func(*Config, *fakeTunnel, *fakeAPI, *Deps)) (*Daemon, *fakeTunnel, *fakeAPI, *fakeLock, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "myst0.conf")

	store := auth.Store{Path: filepath.Join(dir, "token.json")}
	if err := store.Save(&auth.Token{AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresIn: 3600}); err != nil {
		t.Fatalf("seed token store: %v", err)
	}

	cfg := Config{
		TokenPath:        store.Path,
		ConfigPath:       cfgPath,
		Interface:        "myst0",
		IPv6:             true,
		WakeLock:         true,
		HandshakeTimeout: 300 * time.Millisecond,
	}
	tun := &fakeTunnel{handshake: time.Now(), exitIP: "203.0.113.7"}
	fapi := &fakeAPI{connectResp: &api.WireguardConnectResponse{
		ID:       "conn-1",
		WGConfig: testTemplate,
		ExitIP:   "203.0.113.7",
		Country:  "gb",
		City:     "gloucester",
		IPType:   "hosting",
	}}
	lock := &fakeLock{}

	deps := &Deps{
		Tunnel: tun,
		APIFor: func(string) Provisioner { return fapi },
		Tokens: &fakeTokens{token: &auth.Token{AccessToken: "test-access"}},
		Store:  store,
		AcquireLock: func(context.Context, string) (Lock, error) {
			return lock, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if tune != nil {
		tune(&cfg, tun, fapi, deps)
	}

	return New(cfg, *deps), tun, fapi, lock, cfgPath
}

func TestConnectBringsTunnelUp(t *testing.T) {
	d, tun, fapi, lock, cfgPath := newTestDaemon(t, nil)

	st, err := d.Connect(context.Background(), "gb", "hosting")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if st.State != ipc.StateConnected {
		t.Errorf("state = %q, want %q", st.State, ipc.StateConnected)
	}
	if !tun.up {
		t.Error("the interface was never brought up")
	}
	if !st.WakeLock {
		t.Error("status does not report a wake lock while connected")
	}
	if lock.released {
		t.Error("wake lock was released while still connected")
	}
	if st.Country != "gb" || st.City != "gloucester" {
		t.Errorf("region = %q/%q, want gb/gloucester", st.Country, st.City)
	}
	if !st.IPv6Routed {
		t.Error("status does not report IPv6 as routed, but ::/0 was added")
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	conf := string(raw)
	if strings.Contains(conf, wg.PrivateKeyPlaceholder) {
		t.Error("the placeholder survived into the written config")
	}
	if !strings.Contains(conf, "::/0") {
		t.Error("IPv6 route was not added to AllowedIPs")
	}

	// The key registered with the server must be the pair of the key we wrote
	// locally, or the tunnel would come up and never handshake.
	priv := fieldValue(conf, "PrivateKey")
	pub, err := wg.PublicKeyFrom(priv)
	if err != nil {
		t.Fatalf("derive public key from written config: %v", err)
	}
	if pub != fapi.lastReq.PublicKey {
		t.Error("the config's private key does not match the public key sent to the server")
	}
	if fapi.lastReq.PublicKey == priv {
		t.Error("the private key was sent to the server")
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
}

func TestConnectRollsBackWhenHandshakeNeverCompletes(t *testing.T) {
	d, tun, fapi, lock, cfgPath := newTestDaemon(t, func(_ *Config, tn *fakeTunnel, _ *fakeAPI, _ *Deps) {
		tn.handshake = time.Time{} // interface up, peer silent
	})

	st, err := d.Connect(context.Background(), "gb", "")
	if err == nil {
		t.Fatal("expected an error when the tunnel never handshakes")
	}
	if st.State != ipc.StateError {
		t.Errorf("state = %q, want %q", st.State, ipc.StateError)
	}
	if tun.downCalls != 1 {
		t.Errorf("interface teardown called %d times, want 1", tun.downCalls)
	}
	if !lock.released {
		t.Error("wake lock was not released during rollback")
	}
	if fapi.disconnects != 1 {
		t.Errorf("server-side disconnect called %d times, want 1", fapi.disconnects)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("the private key was left on disk after a failed connect")
	}
}

func TestConnectIsIdempotent(t *testing.T) {
	d, tun, _, _, _ := newTestDaemon(t, nil)

	if _, err := d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if _, err := d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if tun.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 — the second connect rebuilt the tunnel", tun.upCalls)
	}
}

func TestConnectToAnotherRegionTearsDownFirst(t *testing.T) {
	d, tun, _, _, _ := newTestDaemon(t, nil)

	if _, err := d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if _, err := d.Connect(context.Background(), "de", ""); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if tun.downCalls != 1 {
		t.Errorf("teardown called %d times before switching region, want 1", tun.downCalls)
	}
	if tun.upCalls != 2 {
		t.Errorf("Up called %d times, want 2", tun.upCalls)
	}
}

func TestDisconnectReleasesEverything(t *testing.T) {
	d, tun, fapi, lock, cfgPath := newTestDaemon(t, nil)

	if _, err := d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	st, err := d.Disconnect(context.Background())
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if st.State != ipc.StateDisconnected {
		t.Errorf("state = %q, want %q", st.State, ipc.StateDisconnected)
	}
	if tun.up {
		t.Error("the interface is still up")
	}
	if !lock.released {
		t.Error("the wake lock outlived the tunnel")
	}
	if fapi.disconnects != 1 {
		t.Errorf("server-side disconnect called %d times, want 1", fapi.disconnects)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("the private key was left on disk after disconnect")
	}
}

func TestStatusDetectsVanishedInterface(t *testing.T) {
	d, tun, _, lock, _ := newTestDaemon(t, nil)

	if _, err := d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Something outside the daemon took the interface down.
	tun.up = false

	st, err := d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != ipc.StateError {
		t.Errorf("state = %q, want %q — a vanished interface must not read as connected", st.State, ipc.StateError)
	}
	if st.LastError == "" {
		t.Error("no explanation was recorded for the dropped tunnel")
	}
	if !lock.released {
		t.Error("the wake lock was held for a tunnel that no longer exists")
	}
}

func TestWakeLockFailureDoesNotBlockConnect(t *testing.T) {
	d, _, _, _, _ := newTestDaemon(t, func(_ *Config, _ *fakeTunnel, _ *fakeAPI, deps *Deps) {
		deps.AcquireLock = func(context.Context, string) (Lock, error) {
			return nil, errors.New("no logind here")
		}
	})

	st, err := d.Connect(context.Background(), "gb", "")
	if err != nil {
		t.Fatalf("a missing wake lock must not fail the connect: %v", err)
	}
	if st.WakeLock {
		t.Error("status claims a wake lock that was never taken")
	}
	if st.State != ipc.StateConnected {
		t.Errorf("state = %q, want %q", st.State, ipc.StateConnected)
	}
}

func TestConnectSurfacesProvisioningFailure(t *testing.T) {
	d, tun, _, _, _ := newTestDaemon(t, func(_ *Config, _ *fakeTunnel, a *fakeAPI, _ *Deps) {
		a.connectErr = errors.New("account has no device slots left")
	})

	st, err := d.Connect(context.Background(), "gb", "")
	if err == nil {
		t.Fatal("expected the provisioning error to surface")
	}
	if st.State != ipc.StateError {
		t.Errorf("state = %q, want %q", st.State, ipc.StateError)
	}
	if !strings.Contains(st.LastError, "no device slots") {
		t.Errorf("LastError = %q, does not explain the failure", st.LastError)
	}
	if tun.upCalls != 0 {
		t.Error("the interface was brought up despite provisioning failing")
	}
}

// fieldValue pulls the right-hand side of the first `field = value` line.
func fieldValue(conf, field string) string {
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, field) {
			if _, value, ok := strings.Cut(trimmed, "="); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}
