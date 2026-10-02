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

// upCount reports how many times the interface was brought up, which is how a
// test tells a rebuilt tunnel from one that was left alone.
func (f *fakeTunnel) upCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upCalls
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
	configResp  *api.ConnectionConfig
	configErr   error
	disconnects int
	lastReq     api.WireguardConnectRequest

	locations       map[string][]api.LocationCountry
	locationsErr    error
	locationFetches int
}

func (f *fakeAPI) Config(context.Context) (*api.ConnectionConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configErr != nil {
		return nil, f.configErr
	}
	return f.configResp, nil
}

// Locations serves the canned list for an ip type, counting the fetches so a
// test can prove the cache is doing its job.
func (f *fakeAPI) Locations(_ context.Context, ipType string) ([]api.LocationCountry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locationFetches++
	if f.locationsErr != nil {
		return nil, f.locationsErr
	}
	key := ipType
	if key == "" {
		key = "residential"
	}
	return f.locations[key], nil
}

// locationFetchCount reports how many times the API was actually asked, which
// is what distinguishes a cache hit from a cache miss.
func (f *fakeAPI) locationFetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locationFetches
}

// failLocationsFrom makes every later location fetch fail.
func (f *fakeAPI) failLocationsFrom(err error) {
	f.mu.Lock()
	f.locationsErr = err
	f.mu.Unlock()
}

func (f *fakeAPI) Connect(_ context.Context, req api.WireguardConnectRequest) (*api.WireguardConnectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	if f.connectErr != nil {
		return nil, f.connectErr
	}

	// Echo back what was asked for, as the real API does — a field the request
	// left empty keeps the canned default, standing in for the server choosing
	// for itself. Without the echo a test cannot tell a client that sent the
	// city from one that dropped it: the response would say "gloucester" either
	// way, and the assertion would pass on a client that never sends a city.
	resp := *f.connectResp
	if req.Country != "" {
		resp.Country = req.Country
	}
	if req.City != "" {
		resp.City = req.City
	}
	if req.IPType != "" {
		resp.IPType = req.IPType
	}
	return &resp, nil
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", IPType: "hosting"})
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"})
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !armedWhenUpRan {
		t.Error("the interface was brought up before the kill switch was armed")
	}
}

func TestDisconnectDisarmsTheKillSwitch(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err == nil {
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"})
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"})
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"})
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if env.tun.upCalls != 1 {
		t.Errorf("Up called %d times, want 1 — the second connect rebuilt the tunnel", env.tun.upCalls)
	}
}

func TestConnectToAnotherRegionTearsDownFirst(t *testing.T) {
	env := newTestDaemon(t, nil)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "de"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"})
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

	st, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"})
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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
		c.StaleAfter = 200 * time.Millisecond
	})

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

// TestMonitorLeavesAHealthyIdleTunnelAlone is the regression for a monitor that
// hung up on a working tunnel every couple of minutes.
//
// The bug was the stale threshold being shorter than WireGuard's own rekey
// cycle. A handshake is not refreshed by traffic: PersistentKeepalive sends a
// packet every 25s, but on an established session those travel inside it and
// leave the handshake timestamp untouched. Only the rekey refreshes it, and
// WireGuard starts that at REKEY_AFTER_TIME (120s), on the first send after —
// so up to one keepalive later.
//
// The result was a handshake age that legitimately reached ~145s on a tunnel
// that was working perfectly, and a 90s threshold that declared it dead. The
// user saw the client disconnect itself reporting "no handshake for 1m43sec".
//
// This drives the monitor with exactly that age and asserts it does nothing.
func TestMonitorLeavesAHealthyIdleTunnelAlone(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) { monitorOn(c) })

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The age a healthy idle tunnel reaches just before its rekey: the rekey
	// interval plus one keepalive. Route and interface are untouched, so the
	// handshake check is the only signal that could fire.
	healthyIdleAge := 120*time.Second + 25*time.Second
	env.tun.setHandshake(time.Now().Add(-healthyIdleAge))

	// Several monitor intervals: a threshold below this age would rebuild on the
	// first one.
	time.Sleep(100 * time.Millisecond)

	if up, _ := env.tun.calls(); up != 1 {
		t.Errorf("Up called %d times, want 1 — the monitor rebuilt a healthy tunnel whose handshake was only %s old",
			up, healthyIdleAge)
	}
	if st := env.d.snapshot(); st.State != ipc.StateConnected {
		t.Errorf("state = %s, want connected — a healthy idle tunnel was torn down", st.State)
	}
}

// TestStaleThresholdOutlastsTheRekeyCycle pins the default against WireGuard's
// timers rather than against a comment.
//
// The threshold is only ever tested with an explicit value — every monitor test
// sets StaleAfter itself — so the default could be, and was, wrong in a way
// nothing caught. These are the protocol's constants, written out because the
// daemon cannot import them from anywhere.
func TestStaleThresholdOutlastsTheRekeyCycle(t *testing.T) {
	const (
		// wg's REKEY_AFTER_TIME: the session age at which a new handshake starts.
		rekeyAfterTime = 120 * time.Second
		// The config's PersistentKeepalive, which bounds the delay before that
		// rekey is triggered by a send.
		persistentKeepalive = 25 * time.Second
		// wg's REJECT_AFTER_TIME: past this the session cannot carry traffic.
		rejectAfterTime = 180 * time.Second
	)

	// A healthy idle tunnel's handshake age peaks here, and anything at or below
	// it tears down working tunnels on a timer.
	if healthyPeak := rekeyAfterTime + persistentKeepalive; DefaultStaleAfter <= healthyPeak {
		t.Errorf("DefaultStaleAfter = %s, which a healthy idle tunnel's handshake age (%s) reaches — the monitor would disconnect a working tunnel",
			DefaultStaleAfter, healthyPeak)
	}

	// And it should not be so eager that it fires while the session is still
	// theoretically usable, or the backstop reports drops that are not drops.
	if DefaultStaleAfter < rejectAfterTime {
		t.Errorf("DefaultStaleAfter = %s, below WireGuard's REJECT_AFTER_TIME (%s) — a session that old may still recover on its own",
			DefaultStaleAfter, rejectAfterTime)
	}
}

func TestMonitorReturnsTheNetworkWhenRecoveryKeepsFailing(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, _ *testEnv, _ *Deps) { monitorOn(c) })

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
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

// TestNoServerInRegionIsExplained is the regression for the failure a first-run
// click on Connect actually hit.
//
// The US had no free WireGuard node. The API reports that as an HTTP 500, and
// the client passed it straight through as "api 500 2332: Could not retrieve
// wireguard connect configuration" — a server fault, from the user's side,
// with nothing to act on. The region is unserved, not broken, and saying which
// one and what to do about it is the whole difference.
func TestNoServerInRegionIsExplained(t *testing.T) {
	env := newTestDaemon(t, func(_ *Config, env *testEnv, _ *Deps) {
		env.api.connectErr = &api.APIError{
			Status:  500,
			Code:    "2332",
			Message: "Could not retrieve wireguard connect configuration",
			// The real body, trimmed only of the UserID.
			Detail: "Could not retrieve wireguard connect configuration: failed to " +
				"find available wireguard server: no available instance for filters: " +
				"{Country:us City: IPType: UserID:test-user UserPlanID:plan_example HashSalt:}",
		}
	})

	_, err := env.d.Connect(context.Background(), ipc.Target{Country: "us"})
	if err == nil {
		t.Fatal("Connect reported success against an API that refused the region")
	}

	msg := err.Error()
	if !strings.Contains(msg, `"us"`) {
		t.Errorf("the error does not name the region that failed: %q", msg)
	}
	if !strings.Contains(msg, "another region") {
		t.Errorf("the error does not tell the user what to do: %q", msg)
	}
	if strings.Contains(msg, "provision tunnel: api 500") {
		t.Errorf("the raw server fault is still the whole message: %q", msg)
	}
}

// TestNoServerAnywhereIsExplained covers the same condition with no country
// asked for, where there is no region to name. The old code reached this path
// only when the API had nothing free at all, which is rare — but the advice
// must still not be "try another region", because none was requested.
func TestNoServerAnywhereIsExplained(t *testing.T) {
	env := newTestDaemon(t, func(_ *Config, env *testEnv, _ *Deps) {
		env.api.connectErr = &api.APIError{
			Status:  500,
			Code:    "2332",
			Message: "Could not retrieve wireguard connect configuration",
			Detail:  "failed to find available wireguard server: no available instance for filters: {Country: City: IPType:}",
		}
	})

	_, err := env.d.Connect(context.Background(), ipc.Target{})
	if err == nil {
		t.Fatal("Connect reported success against an API that refused the region")
	}
	if msg := err.Error(); !strings.Contains(msg, "try again") {
		t.Errorf("the error gives the user nothing to do: %q", msg)
	}
}

// --- Cities ---------------------------------------------------------------

// locationFixture is a small stand-in for the API's location list: two
// countries, one of them with cities and one without, matching the real
// response's shape.
func locationFixture() map[string][]api.LocationCountry {
	return map[string][]api.LocationCountry{
		"residential": {
			{
				Country:      "GB",
				Translations: map[string]string{"en": "United Kingdom", "de": "Vereinigtes Königreich"},
				Cities: []api.LocationCity{
					{City: "london", Translations: map[string]string{"en": "London"}},
					{City: "manchester", Translations: map[string]string{"en": "Manchester"}},
				},
			},
			{
				// The API really does serve a country with no cities listed.
				Country:      "AD",
				Translations: map[string]string{"en": "Andorra"},
			},
		},
		"hosting": {
			{
				Country:      "GB",
				Translations: map[string]string{"en": "United Kingdom"},
				Cities: []api.LocationCity{
					{City: "london", Translations: map[string]string{"en": "London"}},
				},
			},
		},
	}
}

func withLocations(t *testing.T) *testEnv {
	t.Helper()
	return newTestDaemon(t, func(_ *Config, env *testEnv, _ *Deps) {
		env.api.locations = locationFixture()
	})
}

func TestConnectSendsTheCityToTheAPI(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", City: "london"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := env.api.lastRequest().City; got != "london" {
		t.Errorf("the API was asked for city %q, want london — picking a city would silently connect to any city in the country", got)
	}
	if got := env.api.lastRequest().Country; got != "gb" {
		t.Errorf("country = %q, want gb", got)
	}
}

// TestPickingADifferentCityRebuildsTheTunnel is the whole point of the city
// picker. If the idempotence check only compared countries, choosing Manchester
// while sitting in London would return the London tunnel and look, to the user,
// like the picker did nothing.
func TestPickingADifferentCityRebuildsTheTunnel(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", City: "london"}); err != nil {
		t.Fatalf("Connect london: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", City: "manchester"}); err != nil {
		t.Fatalf("Connect manchester: %v", err)
	}

	if got := env.api.lastRequest().City; got != "manchester" {
		t.Errorf("city = %q, want manchester", got)
	}
	if up := env.tun.upCount(); up != 2 {
		t.Errorf("the interface was brought up %d times, want 2 — the tunnel was not rebuilt for the new city", up)
	}
}

// TestReconnectingToTheSameCityIsANoOp keeps connecting idempotent, which is
// what stops a front end's poll from tearing down a working tunnel.
func TestReconnectingToTheSameCityIsANoOp(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", City: "london"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "GB", City: "london"}); err != nil {
		t.Fatalf("Connect again: %v", err)
	}

	if up := env.tun.upCount(); up != 1 {
		t.Errorf("the interface was brought up %d times, want 1 — a repeat connect rebuilt a working tunnel", up)
	}
}

// TestSwitchingIPTypeRebuildsTheTunnel covers a quieter bug: residential and
// hosting are different pools, and asking for one while connected on the other
// used to be ignored entirely.
func TestSwitchingIPTypeRebuildsTheTunnel(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", IPType: "residential"}); err != nil {
		t.Fatalf("Connect residential: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", IPType: "hosting"}); err != nil {
		t.Fatalf("Connect hosting: %v", err)
	}

	if got := env.api.lastRequest().IPType; got != "hosting" {
		t.Errorf("ip type = %q, want hosting — switching pools was silently ignored", got)
	}
}

// TestLeavingTheCityUnsetIsAPreferenceNotADemand pins the wildcard rule: asking
// for "any city in GB" while sitting in GB/London is a match, because an
// unexpressed preference is not a request to move.
func TestLeavingTheCityUnsetIsAPreferenceNotADemand(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", City: "london"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("Connect without a city: %v", err)
	}

	if up := env.tun.upCount(); up != 1 {
		t.Errorf("the interface was brought up %d times, want 1 — clearing a city preference rebuilt the tunnel", up)
	}
}

func TestLocationsAreTrimmedForThePicker(t *testing.T) {
	env := withLocations(t)

	locs, err := env.d.Locations(context.Background(), "")
	if err != nil {
		t.Fatalf("Locations: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("got %d countries, want 2", len(locs))
	}

	gb := locs[0]
	if gb.Code != "GB" {
		t.Errorf("code = %q, want GB", gb.Code)
	}
	// The English label, not the raw code and not the German translation.
	if gb.Name != "United Kingdom" {
		t.Errorf("name = %q, want the English label", gb.Name)
	}
	if len(gb.Cities) != 2 {
		t.Fatalf("got %d cities, want 2", len(gb.Cities))
	}
	if gb.Cities[0].Slug != "london" || gb.Cities[0].Name != "London" {
		t.Errorf("city = %+v, want the slug and its English name", gb.Cities[0])
	}

	// A country with no cities still has to appear: dropping it would make it
	// unselectable, when connecting to the country alone is perfectly valid.
	if ad := locs[1]; ad.Code != "AD" || len(ad.Cities) != 0 {
		t.Errorf("the city-less country = %+v, want AD with no cities", ad)
	}
}

// TestLocationListIsCached is the behaviour the picker depends on. The
// residential list is about a megabyte; fetching it every time a dropdown opens
// would be slow and rude to the API.
func TestLocationListIsCached(t *testing.T) {
	env := withLocations(t)

	for i := 0; i < 4; i++ {
		if _, err := env.d.Locations(context.Background(), "residential"); err != nil {
			t.Fatalf("Locations call %d: %v", i, err)
		}
	}
	if got := env.api.locationFetchCount(); got != 1 {
		t.Errorf("the API was asked %d times for the same list, want 1 — the cache is not being used", got)
	}

	// The other dataset is a different list, not a filtered view of the first,
	// so it must be fetched in its own right.
	if _, err := env.d.Locations(context.Background(), "hosting"); err != nil {
		t.Fatalf("Locations(hosting): %v", err)
	}
	if got := env.api.locationFetchCount(); got != 2 {
		t.Errorf("fetch count = %d, want 2 — the two ip types should not share a cache entry", got)
	}
}

// TestTheDefaultIPTypeIsCachedUnderItsRealName guards a cache that looks like
// it works but fetches twice: the API treats an absent ip_type as residential,
// so asking with "" and with "residential" is the same request.
func TestTheDefaultIPTypeIsCachedUnderItsRealName(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Locations(context.Background(), ""); err != nil {
		t.Fatalf("Locations(\"\"): %v", err)
	}
	if _, err := env.d.Locations(context.Background(), "residential"); err != nil {
		t.Fatalf("Locations(residential): %v", err)
	}

	if got := env.api.locationFetchCount(); got != 1 {
		t.Errorf("fetch count = %d, want 1 — an empty ip type and \"residential\" are the same list", got)
	}
}

// TestAStaleListBeatsNoList covers the API having a bad minute. Emptying the
// picker is a worse failure than offering a city that was retired this morning,
// and the connect call still has the final say on whether a city exists.
func TestAStaleListBeatsNoList(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Locations(context.Background(), "residential"); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	// Expire the entry and break the API.
	env.d.locsMu.Lock()
	entry := env.d.locs["residential"]
	entry.fetched = time.Now().Add(-2 * DefaultLocationsTTL)
	env.d.locs["residential"] = entry
	env.d.locsMu.Unlock()
	env.api.failLocationsFrom(errors.New("api is down"))

	locs, err := env.d.Locations(context.Background(), "residential")
	if err != nil {
		t.Fatalf("a cached list should have been served: %v", err)
	}
	if len(locs) != 2 {
		t.Errorf("got %d countries from the cache, want 2", len(locs))
	}
}

// TestNoCacheAndNoAPIIsAnError is the other half: with nothing cached there is
// nothing to serve, and the caller has to hear about it rather than being
// handed an empty picker that looks like an account with no countries.
func TestNoCacheAndNoAPIIsAnError(t *testing.T) {
	env := newTestDaemon(t, func(_ *Config, env *testEnv, _ *Deps) {
		env.api.locationsErr = errors.New("api is down")
	})

	if _, err := env.d.Locations(context.Background(), "residential"); err == nil {
		t.Fatal("an empty result was reported as success")
	}
}

// TestSwitchingAccountsDropsTheCachedCities keeps a per-account list from
// outliving the account. The new account would otherwise be offered the old
// one's cities, which fails at connect time with no hint as to why.
func TestSwitchingAccountsDropsTheCachedCities(t *testing.T) {
	env := withLocations(t)

	if _, err := env.d.Locations(context.Background(), "residential"); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	if got := env.api.locationFetchCount(); got != 1 {
		t.Fatalf("fetch count = %d, want 1", got)
	}

	if _, err := env.d.Login(context.Background(), []byte(`{"access_token":"other-account"}`)); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := env.d.Locations(context.Background(), "residential"); err != nil {
		t.Fatalf("Locations after login: %v", err)
	}
	if got := env.api.locationFetchCount(); got != 2 {
		t.Errorf("fetch count = %d, want 2 — the previous account's cities survived the account switch", got)
	}
}

// TestRecoveryRebuildsTheSameCity keeps the health monitor from quietly moving
// the user. A tunnel that dies in Manchester must come back in Manchester, not
// anywhere in the country the monitor happened to record.
func TestRecoveryRebuildsTheSameCity(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, env *testEnv, _ *Deps) {
		monitorOn(c)
		env.api.locations = locationFixture()
	})

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb", City: "manchester"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The tunnel stops answering.
	env.tun.setHandshake(time.Now().Add(-time.Hour))
	waitFor(t, 3*time.Second, "the monitor to rebuild the tunnel", func() bool {
		return env.tun.upCount() >= 2
	})

	if got := env.api.lastRequest().City; got != "manchester" {
		t.Errorf("the recovery asked for city %q, want manchester — the tunnel came back somewhere the user did not choose", got)
	}
}

// TestRecoveryOfACityLessConnectStaysBroad is the other half of the rule above,
// and the reason it is a rule rather than a coincidence.
//
// Asked for a country and nothing more, the API assigns whichever city it
// likes — "gloucester" in the fixture. A recovery that echoed that back would
// pin the reconnect to one city, so a country with a hundred free cities could
// fail to recover because that one was full. The user never chose it, so it
// should not narrow anything.
//
// With the API echoing what it is asked for, this cannot be told apart from the
// test above by inspecting the response alone; it is the *request* that has to
// stay empty.
func TestRecoveryOfACityLessConnectStaysBroad(t *testing.T) {
	env := newTestDaemon(t, func(c *Config, env *testEnv, _ *Deps) {
		monitorOn(c)
		env.api.locations = locationFixture()
	})

	if _, err := env.d.Connect(context.Background(), ipc.Target{Country: "gb"}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// The API chose for us, and said so.
	if st := env.d.snapshot(); st.City == "" {
		t.Fatal("the fixture assigned no city, so this proves nothing")
	}

	env.tun.setHandshake(time.Now().Add(-time.Hour))
	waitFor(t, 3*time.Second, "the monitor to rebuild the tunnel", func() bool {
		return env.tun.upCount() >= 2
	})

	if got := env.api.lastRequest().City; got != "" {
		t.Errorf("the recovery asked for city %q — a city the user never chose was pinned onto the reconnect, narrowing it for no reason", got)
	}
	if got := env.api.lastRequest().Country; got != "gb" {
		t.Errorf("the recovery asked for country %q, want gb", got)
	}
}
