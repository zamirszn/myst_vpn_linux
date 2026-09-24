package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/api"
	"github.com/zamirszn/mysterium-linux-client/internal/auth"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
	"github.com/zamirszn/mysterium-linux-client/internal/killswitch"
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

const (
	testEndpointIP   = "91.98.228.210"
	testEndpointPort = 56666
)

// fakeTunnel is guarded by a mutex because the health monitor drives it from
// its own goroutine once a connection is up.
type fakeTunnel struct {
	mu           sync.Mutex
	up           bool
	upCalls      int
	downCalls    int
	upErr        error
	handshake    time.Time
	handshakeErr error
	exitIP       string
	exitErr      error
	route        wg.Route
	routeErr     error

	// noHandshake models an interface that comes up and never reaches its
	// peer, which is the failure the connect path has to roll back.
	noHandshake bool
}

func (f *fakeTunnel) Up(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upCalls++
	if f.upErr != nil {
		return f.upErr
	}
	f.up = true
	// A freshly created interface handshakes with its peer, which is what the
	// daemon waits for before it will call the tunnel up. Refreshing here keeps
	// that true across a reconnect, so a recovery is not immediately seen as
	// dead again by the very monitor that just performed it.
	if !f.noHandshake {
		f.handshake = time.Now()
	}
	return nil
}

// neverHandshakes models an interface that comes up and stays silent.
func (f *fakeTunnel) neverHandshakes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handshake = time.Time{}
	f.noHandshake = true
}

func (f *fakeTunnel) Down(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downCalls++
	f.up = false
	return nil
}

func (f *fakeTunnel) IsUp(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.up
}

func (f *fakeTunnel) Handshake(context.Context) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handshake, f.handshakeErr
}

func (f *fakeTunnel) ExitIP(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitIP, f.exitErr
}

func (f *fakeTunnel) DefaultRoute(context.Context) (wg.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.route, f.routeErr
}

// calls reports how many times the interface was brought up and down.
func (f *fakeTunnel) calls() (up, down int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upCalls, f.downCalls
}

// interfaceUp reports whether the fake interface currently exists.
func (f *fakeTunnel) interfaceUp() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.up
}

// setInterface simulates the interface appearing or disappearing behind the
// daemon's back.
func (f *fakeTunnel) setInterface(up bool) {
	f.mu.Lock()
	f.up = up
	f.mu.Unlock()
}

// setRoute simulates the machine moving to a different network.
func (f *fakeTunnel) setRoute(r wg.Route) {
	f.mu.Lock()
	f.route = r
	f.mu.Unlock()
}

// setHandshake backdates the last handshake, as a dropped tunnel would.
func (f *fakeTunnel) setHandshake(at time.Time) {
	f.mu.Lock()
	f.handshake = at
	f.mu.Unlock()
}

type fakeAPI struct {
	mu          sync.Mutex
	connectResp *api.WireguardConnectResponse
	connectErr  error
	disconnects int
	lastReq     api.WireguardConnectRequest
}

func (f *fakeAPI) Connect(_ context.Context, req api.WireguardConnectRequest) (*api.WireguardConnectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	return f.connectResp, nil
}

func (f *fakeAPI) Disconnect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects++
	return nil
}

// disconnectCount reports how many times the server-side connection was given up.
func (f *fakeAPI) disconnectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disconnects
}

// lastRequest reports the most recent provisioning request.
func (f *fakeAPI) lastRequest() api.WireguardConnectRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

// failConnectFrom makes every later provisioning attempt fail, as a node that
// has gone away would.
func (f *fakeAPI) failConnectFrom(err error) {
	f.mu.Lock()
	f.connectErr = err
	f.mu.Unlock()
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

type fakeLock struct {
	mu       sync.Mutex
	released bool
	err      error
}

func (l *fakeLock) Release() {
	l.mu.Lock()
	l.released = true
	l.mu.Unlock()
}

func (l *fakeLock) wasReleased() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

type fakeKillSwitch struct {
	mu         sync.Mutex
	installed  bool
	installs   int
	removes    int
	installErr error
	spec       killswitch.Spec
}

func (f *fakeKillSwitch) Install(_ context.Context, spec killswitch.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	if f.installErr != nil {
		return f.installErr
	}
	f.installed = true
	f.spec = spec
	return nil
}

func (f *fakeKillSwitch) Remove(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	f.installed = false
	return nil
}

func (f *fakeKillSwitch) Active(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed, nil
}

// armed reports whether the egress filter is currently installed.
func (f *fakeKillSwitch) armed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed
}

// removeCount reports how many times the filter was disarmed.
func (f *fakeKillSwitch) removeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removes
}

// testEnv holds everything a test needs to inspect after driving the daemon.
type testEnv struct {
	d       *Daemon
	tun     *fakeTunnel
	api     *fakeAPI
	lock    *fakeLock
	ks      *fakeKillSwitch
	cfgPath string
}

// newTestDaemon builds a daemon wired entirely to fakes. tune may adjust any
// piece before construction.
func newTestDaemon(t *testing.T, tune func(*Config, *testEnv, *Deps)) *testEnv {
	t.Helper()
	dir := t.TempDir()

	store := auth.Store{Path: filepath.Join(dir, "token.json")}
	if err := store.Save(&auth.Token{AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresIn: 3600}); err != nil {
		t.Fatalf("seed token store: %v", err)
	}

	cfg := Config{
		TokenPath:        store.Path,
		ConfigPath:       filepath.Join(dir, "myst0.conf"),
		Interface:        "myst0",
		IPv6:             true,
		WakeLock:         true,
		KillSwitch:       true,
		HandshakeTimeout: 300 * time.Millisecond,
	}
	env := &testEnv{
		tun: &fakeTunnel{
			handshake: time.Now(),
			exitIP:    "203.0.113.7",
			route:     wg.Route{Gateway: "10.0.0.1", Interface: "wlo1"},
		},
		lock:    &fakeLock{},
		ks:      &fakeKillSwitch{},
		cfgPath: cfg.ConfigPath,
	}
	env.api = &fakeAPI{connectResp: &api.WireguardConnectResponse{
		ID:       "conn-1",
		WGConfig: testTemplate,
		ExitIP:   "203.0.113.7",
		Country:  "gb",
		City:     "gloucester",
		IPType:   "hosting",
	}}

	deps := &Deps{
		Tunnel: env.tun,
		APIFor: func(string) Provisioner { return env.api },
		Tokens: &fakeTokens{token: &auth.Token{AccessToken: "test-access"}},
		Store:  store,
		AcquireLock: func(context.Context, string) (Lock, error) {
			return env.lock, nil
		},
		KillSwitch: env.ks,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if tune != nil {
		tune(&cfg, env, deps)
	}

	env.d = New(cfg, *deps)
	env.cfgPath = cfg.ConfigPath
	return env
}

func TestConnectBringsTunnelUp(t *testing.T) {
	env := newTestDaemon(t, nil)

	st, err := env.d.Connect(context.Background(), "gb", "hosting")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if st.State != ipc.StateConnected {
		t.Errorf("state = %q, want %q", st.State, ipc.StateConnected)
	}
	if !env.tun.up {
		t.Error("the interface was never brought up")
	}
	if !st.WakeLock {
		t.Error("status does not report a wake lock while connected")
	}
	if env.lock.released {
		t.Error("wake lock was released while still connected")
	}
	if st.Country != "gb" || st.City != "gloucester" {
		t.Errorf("region = %q/%q, want gb/gloucester", st.Country, st.City)
	}
	if !st.IPv6Routed {
		t.Error("status does not report IPv6 as routed, but ::/0 was added")
	}

	raw, err := os.ReadFile(env.cfgPath)
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
	if pub != env.api.lastReq.PublicKey {
		t.Error("the config's private key does not match the public key sent to the server")
	}
	if env.api.lastReq.PublicKey == priv {
		t.Error("the private key was sent to the server")
	}

	info, err := os.Stat(env.cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}
}

func TestConnectArmsTheKillSwitch(t *testing.T) {
	env := newTestDaemon(t, nil)

	st, err := env.d.Connect(context.Background(), "gb", "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !st.KillSwitch {
		t.Error("status does not report the kill switch as armed")
	}
	if !env.ks.installed {
		t.Fatal("the kill switch was never installed")
	}
	if env.ks.spec.Interface != "myst0" {
		t.Errorf("filter interface = %q, want myst0", env.ks.spec.Interface)
	}
	if len(env.ks.spec.Endpoints) != 1 {
		t.Fatalf("filter has %d endpoints, want 1", len(env.ks.spec.Endpoints))
	}
	got := env.ks.spec.Endpoints[0]
	if got.IP != testEndpointIP || got.Port != testEndpointPort {
		t.Errorf("allowed endpoint = %s:%d, want %s:%d", got.IP, got.Port, testEndpointIP, testEndpointPort)
	}
}

func TestKillSwitchIsArmedBeforeTheInterfaceComesUp(t *testing.T) {
	// The ordering is the whole point: arm first, so there is never a moment
	// where the tunnel is expected but traffic could still leave in the clear.
	var armedWhenUpRan bool
	env := newTestDaemon(t, func(_ *Config, e *testEnv, deps *Deps) {
		deps.Tunnel = &trackingTunnel{inner: e.tun, onUp: func() { armedWhenUpRan = e.ks.installed }}
	})

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !armedWhenUpRan {
		t.Error("the interface was brought up before the kill switch was armed")
	}
}

func TestDisconnectDisarmsTheKillSwitch(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	st, err := env.d.Disconnect(context.Background())
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if st.KillSwitch {
		t.Error("status still reports the kill switch as armed after disconnect")
	}
	if env.ks.installed {
		t.Error("the egress filter survived the disconnect")
	}
}

func TestFailedConnectDisarmsTheKillSwitch(t *testing.T) {
	// A connect that arms the filter and then fails would otherwise leave the
	// machine with no network and nothing left to remove it.
	env := newTestDaemon(t, func(_ *Config, e *testEnv, _ *Deps) {
		e.tun.neverHandshakes()
	})

	if _, err := env.d.Connect(context.Background(), "gb", ""); err == nil {
		t.Fatal("expected the connect to fail")
	}
	if env.ks.installed {
		t.Error("the egress filter was left armed after a failed connect")
	}
	if env.ks.removes == 0 {
		t.Error("the kill switch was never disarmed")
	}
}

func TestKillSwitchFailureAbortsTheConnect(t *testing.T) {
	// Failing closed matters: if the filter cannot be installed, proceeding
	// would silently leave traffic unprotected.
	env := newTestDaemon(t, func(_ *Config, e *testEnv, _ *Deps) {
		e.ks.installErr = errors.New("nft: permission denied")
	})

	st, err := env.d.Connect(context.Background(), "gb", "")
	if err == nil {
		t.Fatal("expected the connect to fail when the kill switch cannot be armed")
	}
	if st.State != ipc.StateError {
		t.Errorf("state = %q, want %q", st.State, ipc.StateError)
	}
	if env.tun.upCalls != 0 {
		t.Error("the interface was brought up despite the kill switch failing")
	}
}

func TestKillSwitchDisabledDoesNotArm(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) {
		c.KillSwitch = false
	})

	st, err := env.d.Connect(context.Background(), "gb", "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if env.ks.installs != 0 {
		t.Error("the kill switch was armed despite being disabled")
	}
	if st.KillSwitch {
		t.Error("status claims a kill switch that was never armed")
	}
}

func TestConnectRollsBackWhenHandshakeNeverCompletes(t *testing.T) {
	env := newTestDaemon(t, func(_ *Config, e *testEnv, _ *Deps) {
		e.tun.neverHandshakes() // interface up, peer silent
	})

	st, err := env.d.Connect(context.Background(), "gb", "")
	if err == nil {
		t.Fatal("expected an error when the tunnel never handshakes")
	}
	if st.State != ipc.StateError {
		t.Errorf("state = %q, want %q", st.State, ipc.StateError)
	}
	if env.tun.downCalls != 1 {
		t.Errorf("interface teardown called %d times, want 1", env.tun.downCalls)
	}
	if !env.lock.released {
		t.Error("wake lock was not released during rollback")
	}
	if env.api.disconnects != 1 {
		t.Errorf("server-side disconnect called %d times, want 1", env.api.disconnects)
	}
	if _, err := os.Stat(env.cfgPath); !os.IsNotExist(err) {
		t.Error("the private key was left on disk after a failed connect")
	}
}

func TestConnectIsIdempotent(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if env.tun.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 — the second connect rebuilt the tunnel", env.tun.upCalls)
	}
}

func TestConnectToAnotherRegionTearsDownFirst(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), "de", ""); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if env.tun.downCalls != 1 {
		t.Errorf("teardown called %d times before switching region, want 1", env.tun.downCalls)
	}
	if env.tun.upCalls != 2 {
		t.Errorf("Up called %d times, want 2", env.tun.upCalls)
	}
}

func TestDisconnectReleasesEverything(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	st, err := env.d.Disconnect(context.Background())
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if st.State != ipc.StateDisconnected {
		t.Errorf("state = %q, want %q", st.State, ipc.StateDisconnected)
	}
	if env.tun.up {
		t.Error("the interface is still up")
	}
	if !env.lock.released {
		t.Error("the wake lock outlived the tunnel")
	}
	if env.api.disconnects != 1 {
		t.Errorf("server-side disconnect called %d times, want 1", env.api.disconnects)
	}
	if _, err := os.Stat(env.cfgPath); !os.IsNotExist(err) {
		t.Error("the private key was left on disk after disconnect")
	}
}

func TestStatusDetectsVanishedInterface(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !env.ks.armed() {
		t.Fatal("the connect did not arm the kill switch")
	}

	// Something outside the daemon took the interface down.
	env.tun.setInterface(false)

	st, err := env.d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// The answer must be immediate and honest: the tunnel is gone, and the
	// daemon is doing something about it rather than reporting a connection it
	// no longer has.
	if st.State != ipc.StateReconnecting {
		t.Fatalf("state = %q, want %q — a vanished interface must not read as connected", st.State, ipc.StateReconnecting)
	}
	if st.LastError == "" {
		t.Error("no explanation was recorded for the dropped tunnel")
	}

	// The repair happens off the request, so its result is waited for rather
	// than returned.
	waitFor(t, 5*time.Second, "the vanished interface to be rebuilt", func() bool {
		return env.d.snapshot().State == ipc.StateConnected
	})

	if up, _ := env.tun.calls(); up < 2 {
		t.Errorf("the interface was brought up %d times, want at least 2 — it was never rebuilt", up)
	}
	if !env.tun.interfaceUp() {
		t.Error("the interface is still down")
	}
	// A rebuilt tunnel gets its filter back. The old code disarmed the switch
	// and gave up, which fixed the stranding by throwing the tunnel away; the
	// fix must not swing the other way and leave a tunnel unguarded.
	if !env.ks.armed() {
		t.Error("the kill switch was not re-armed over the rebuilt tunnel")
	}
	// A recovery holds the wake lock throughout, or the machine can suspend
	// between the teardown and the rebuild and never wake up connected.
	if env.lock.wasReleased() {
		t.Error("the wake lock was released mid-recovery")
	}
}

// TestStatusDoesNotPreemptTheMonitor is the regression for the bug the live
// drop test found.
//
// Polling the status during a drop used to open the give-up path: the daemon
// tore the connection down and published an error. The monitor ticks on a
// ten-second timer, so anyone watching the state — the first thing a person
// does when the network stops working — destroyed the recovery seconds before
// it would have started, and the machine was left with no tunnel.
func TestStatusDoesNotPreemptTheMonitor(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) {
		monitorOn(c)
		// A realistic interval: long enough that the poll below lands first,
		// which is the ordering that mattered.
		c.MonitorInterval = time.Hour
	})

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	env.tun.setInterface(false)

	// The user's status poll, arriving before any monitor tick could.
	st, err := env.d.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State == ipc.StateError {
		t.Fatal("a status poll gave up on the tunnel — this is the bug the live test found")
	}

	// It must still be repaired, by whoever got there first.
	waitFor(t, 5*time.Second, "the tunnel to come back", func() bool {
		return env.d.snapshot().State == ipc.StateConnected
	})
	if !env.tun.interfaceUp() {
		t.Error("the tunnel was never rebuilt")
	}
	if !env.ks.armed() {
		t.Error("the kill switch was not re-armed over the rebuilt tunnel")
	}
}

func TestWakeLockFailureDoesNotBlockConnect(t *testing.T) {
	env := newTestDaemon(t, func(_ *Config, _ *testEnv, deps *Deps) {
		deps.AcquireLock = func(context.Context, string) (Lock, error) {
			return nil, errors.New("no logind here")
		}
	})

	st, err := env.d.Connect(context.Background(), "gb", "")
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
	env := newTestDaemon(t, func(_ *Config, e *testEnv, _ *Deps) {
		e.api.connectErr = errors.New("account has no device slots left")
	})

	st, err := env.d.Connect(context.Background(), "gb", "")
	if err == nil {
		t.Fatal("expected the provisioning error to surface")
	}
	if st.State != ipc.StateError {
		t.Errorf("state = %q, want %q", st.State, ipc.StateError)
	}
	if !strings.Contains(st.LastError, "no device slots") {
		t.Errorf("LastError = %q, does not explain the failure", st.LastError)
	}
	if env.tun.upCalls != 0 {
		t.Error("the interface was brought up despite provisioning failing")
	}
	if env.ks.installs != 0 {
		t.Error("the kill switch was armed despite provisioning failing")
	}
}

// --- health monitor ---------------------------------------------------------

// monitorOn turns the health monitor on with a short interval, so a test does
// not have to sit through the ten-second default between checks.
func monitorOn(c *Config) {
	c.Monitor = true
	c.MonitorInterval = 5 * time.Millisecond
}

// waitFor polls cond until it holds. The monitor runs on its own goroutine, so
// its effects have to be observed rather than returned to the test.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestMonitorReconnectsAfterTheNetworkChanges(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) { monitorOn(c) })

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The machine moves to a different wifi network. The interface stays up —
	// it is virtual — so the changed default route is the only evidence there
	// is, and the handshake would not look stale for another minute yet.
	env.tun.setRoute(wg.Route{Gateway: "192.168.1.1", Interface: "wlo1"})

	waitFor(t, 5*time.Second, "the monitor to rebuild the tunnel", func() bool {
		up, _ := env.tun.calls()
		return up == 2
	})
	waitFor(t, 5*time.Second, "the tunnel to read as connected again", func() bool {
		return env.d.snapshot().State == ipc.StateConnected
	})

	if !env.tun.interfaceUp() {
		t.Error("the interface is not up after a recovery")
	}
	if got := env.api.lastRequest().Country; got != "gb" {
		t.Errorf("recovery connected to %q, want gb — a recovery must not move the user to another region", got)
	}
	if st := env.d.snapshot(); st.LastError != "" {
		t.Errorf("LastError = %q, want it cleared after a successful recovery", st.LastError)
	}
}

func TestMonitorReconnectsAfterTheHandshakeGoesStale(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) {
		monitorOn(c)
		c.StaleAfter = 50 * time.Millisecond
	})

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The route is unchanged, so the fast path cannot see this: the peer simply
	// stopped answering. With a 25-second keepalive, a handshake an hour old
	// means the tunnel is gone.
	env.tun.setHandshake(time.Now().Add(-time.Hour))

	waitFor(t, 5*time.Second, "the monitor to rebuild the tunnel", func() bool {
		up, _ := env.tun.calls()
		return up == 2
	})

	// A recovery refreshes the handshake, so this must settle rather than spin.
	time.Sleep(50 * time.Millisecond)
	if up, _ := env.tun.calls(); up != 2 {
		t.Errorf("Up called %d times, want 2 — the monitor is rebuilding a healthy tunnel", up)
	}
}

func TestMonitorReturnsTheNetworkWhenRecoveryKeepsFailing(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) { monitorOn(c) })

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !env.ks.armed() {
		t.Fatal("the connect did not arm the kill switch")
	}

	// The network moved and the node is unreachable, so every reconnect fails.
	env.tun.setRoute(wg.Route{Gateway: "192.168.1.1", Interface: "wlo1"})
	env.api.failConnectFrom(errors.New("node unreachable"))

	waitFor(t, 10*time.Second, "the monitor to give up and return the network", func() bool {
		return env.d.snapshot().State == ipc.StateError
	})

	st := env.d.snapshot()
	if !strings.Contains(st.LastError, "kill switch has been disarmed") {
		t.Errorf("LastError = %q, does not tell the user the network is back", st.LastError)
	}
	// The whole point of the fall-back: the machine must not be left filtered
	// with no tunnel to carry its traffic.
	if env.ks.armed() {
		t.Error("the kill switch was left armed after recovery failed — the machine would have no network")
	}
	if !env.lock.wasReleased() {
		t.Error("the wake lock was left held after recovery failed")
	}
	if env.tun.interfaceUp() {
		t.Error("the interface was left up after recovery failed")
	}
}

func TestMonitorStaysOffWhenDisabled(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) {
		c.Monitor = false
		c.MonitorInterval = 5 * time.Millisecond
	})

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	env.tun.setRoute(wg.Route{Gateway: "192.168.1.1", Interface: "wlo1"})

	time.Sleep(100 * time.Millisecond)
	if up, _ := env.tun.calls(); up != 1 {
		t.Errorf("Up called %d times, want 1 — the monitor ran despite being disabled", up)
	}
}

func TestConnectStartsTheMonitorAndDisconnectStopsIt(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) { monitorOn(c) })

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !env.monitorRunning() {
		t.Fatal("Connect did not start the health monitor")
	}

	if _, err := env.d.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if env.monitorRunning() {
		t.Error("Disconnect left the health monitor running for a tunnel that is gone")
	}
}

func TestStatusDoesNotWaitForAnOperationInFlight(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), "gb", ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Stand in for a connect or a recovery holding the operation lock for as
	// long as it takes, which in the worst case is minutes. Status must not
	// wait for it: someone running `myst status` while a reconnect grinds
	// through its attempts is exactly the person who needs an answer.
	env.d.opMu.Lock()
	defer env.d.opMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := env.d.Status(context.Background()); err != nil {
			t.Errorf("Status: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Status blocked behind an operation in flight")
	}
}

// monitorRunning reports whether a health monitor goroutine is registered.
func (e *testEnv) monitorRunning() bool {
	e.d.mu.Lock()
	defer e.d.mu.Unlock()
	return e.d.monitorCancel != nil
}

// trackingTunnel wraps a fakeTunnel to observe the order in which operations
// happen. It goes on Deps.Tunnel; env.tun still points at the inner fake, so
// the usual call-count assertions keep working.
type trackingTunnel struct {
	inner *fakeTunnel
	onUp  func()
}

func (t *trackingTunnel) Up(ctx context.Context, path string) error {
	if t.onUp != nil {
		t.onUp()
	}
	return t.inner.Up(ctx, path)
}
func (t *trackingTunnel) Down(ctx context.Context) error             { return t.inner.Down(ctx) }
func (t *trackingTunnel) IsUp(ctx context.Context) bool              { return t.inner.IsUp(ctx) }
func (t *trackingTunnel) ExitIP(ctx context.Context) (string, error) { return t.inner.ExitIP(ctx) }
func (t *trackingTunnel) Handshake(ctx context.Context) (time.Time, error) {
	return t.inner.Handshake(ctx)
}
func (t *trackingTunnel) DefaultRoute(ctx context.Context) (wg.Route, error) {
	return t.inner.DefaultRoute(ctx)
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
