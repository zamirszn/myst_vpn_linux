// Package daemon owns the tunnel lifecycle: authenticate, provision, bring the
// interface up, verify it actually carries traffic, and tear it all down again.
//
// Everything that touches the outside world sits behind an interface (Tunnel,
// Provisioner, TokenSource, LockFunc) so the state machine can be exercised in
// tests without root, a network, or a real WireGuard interface. The daemon is
// deliberately thin: it sequences operations and owns the invariants, while the
// interesting mechanics live in the packages it calls.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/api"
	"github.com/zamirszn/mysterium-linux-client/internal/auth"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
	"github.com/zamirszn/mysterium-linux-client/internal/killswitch"
	"github.com/zamirszn/mysterium-linux-client/internal/wakelock"
	"github.com/zamirszn/mysterium-linux-client/internal/wg"
)

// Tunnel is the privileged data plane.
type Tunnel interface {
	Up(ctx context.Context, configPath string) error
	Down(ctx context.Context) error
	IsUp(ctx context.Context) bool
	Handshake(ctx context.Context) (time.Time, error)
	ExitIP(ctx context.Context) (string, error)

	// DefaultRoute reports the machine's own egress path, which is how the
	// health monitor notices that the network underneath the tunnel changed.
	DefaultRoute(ctx context.Context) (wg.Route, error)
}

// Provisioner is the control plane, bound to one access token.
type Provisioner interface {
	Connect(ctx context.Context, req api.WireguardConnectRequest) (*api.WireguardConnectResponse, error)
	Disconnect(ctx context.Context) error

	// Config reports the countries the account can reach. It is part of this
	// interface rather than a separate dependency because it is the same
	// authenticated client, built from the same token.
	Config(ctx context.Context) (*api.ConnectionConfig, error)

	// Locations reports the countries and cities the account can reach, for the
	// same reason and by the same client.
	Locations(ctx context.Context, ipType string) ([]api.LocationCountry, error)
}

// APIFor builds a Provisioner for an access token. A function rather than a
// value because the token is not known until it has been refreshed.
type APIFor func(accessToken string) Provisioner

// TokenSource yields a valid token, refreshing it when it is close to expiry.
type TokenSource interface {
	EnsureValid(ctx context.Context, store auth.Store, margin time.Duration) (*auth.Token, error)
}

// Lock is a held wake lock.
type Lock interface{ Release() }

// LockFunc acquires a wake lock.
type LockFunc func(ctx context.Context, why string) (Lock, error)

// KillSwitch arms and disarms the egress filter that keeps traffic inside the
// tunnel when the tunnel is down.
type KillSwitch interface {
	Install(ctx context.Context, spec killswitch.Spec) error
	Remove(ctx context.Context) error
	Active(ctx context.Context) (bool, error)
}

// WGTunnel adapts the wg package to the Tunnel interface.
type WGTunnel struct{}

func (WGTunnel) Up(ctx context.Context, configPath string) error { return wg.Up(ctx, configPath) }
func (WGTunnel) Down(ctx context.Context) error                  { return wg.Down(ctx) }
func (WGTunnel) IsUp(ctx context.Context) bool                   { return wg.IsUp(ctx) }
func (WGTunnel) ExitIP(ctx context.Context) (string, error)      { return wg.ExitIP(ctx) }
func (WGTunnel) Handshake(ctx context.Context) (time.Time, error) {
	return wg.Handshake(ctx)
}
func (WGTunnel) DefaultRoute(ctx context.Context) (wg.Route, error) { return wg.DefaultRoute(ctx) }

// Defaults applied where Config leaves a field zero.
const (
	DefaultTokenPath     = "/var/lib/mystd/token.json"
	DefaultConfigPath    = "/etc/wireguard/myst0.conf"
	DefaultRefreshMargin = 24 * time.Hour

	// DefaultHandshakeTimeout bounds how long we wait for positive evidence that
	// the tunnel works before declaring success.
	DefaultHandshakeTimeout = 10 * time.Second

	// DefaultMonitorInterval is how often the health monitor inspects a live
	// tunnel. Ten seconds is the resolution on noticing a drop, and the check
	// itself is two cheap local commands.
	DefaultMonitorInterval = 10 * time.Second

	// DefaultStaleAfter is how old the last handshake may be before the tunnel
	// is presumed dead.
	//
	// This is WireGuard's own REJECT_AFTER_TIME (180s) — the age at which the
	// protocol itself declares a session unusable and refuses to pass traffic
	// through it. Past that point there is no ambiguity left to resolve: the
	// tunnel is not slow, it is dead.
	//
	// It must be at least that large, and the reason is a trap worth spelling
	// out. A handshake is NOT refreshed by traffic. PersistentKeepalive = 25
	// sends a packet every 25s, but on an established session those keepalives
	// travel inside the existing session and leave its handshake timestamp
	// alone. What refreshes it is the rekey, which WireGuard initiates only once
	// the session reaches REKEY_AFTER_TIME (120s) — and then not until the next
	// packet is sent, so up to 25s later.
	//
	// So a perfectly healthy idle tunnel has a handshake age that climbs to
	// ~145s and then drops back to zero. Any threshold below that sawtooth tears
	// down a working tunnel on a timer: this was 90s, justified at the time as
	// "three and a half keepalive intervals", which reasoned about the wrong
	// timer entirely. The user-visible result was a client that disconnected
	// itself every couple of minutes reporting "no handshake for 1m43sec".
	//
	// The cost of being conservative is detection latency on the one signal this
	// is a backstop for; the cost of being eager is hanging up on a working
	// tunnel. The fast signals — a changed default route, a vanished interface —
	// catch the common failures within one interval regardless.
	DefaultStaleAfter = 180 * time.Second

	// DefaultMaxRecoveryAttempts and DefaultRecoveryWindow bound the reconnect
	// loop. Both are limits on the same thing from different angles — attempts
	// caps the work, the window caps how long the user waits — and the first to
	// run out ends it.
	DefaultMaxRecoveryAttempts = 3
	DefaultRecoveryWindow      = 2 * time.Minute
)

// Config is the daemon's tunable surface.
type Config struct {
	BaseURL    string
	TokenPath  string
	ConfigPath string
	Interface  string

	// IPv6 appends ::/0 to AllowedIPs. Mysterium's config assigns an IPv6
	// address but routes no IPv6, so without this, IPv6 traffic on an
	// IPv6-capable host bypasses the tunnel entirely. Defaults on.
	IPv6 bool

	// WakeLock inhibits suspend while connected. Defaults on.
	WakeLock bool

	// KillSwitch installs an nftables egress filter while connected, so that a
	// dropped tunnel blocks traffic rather than letting it escape in the clear.
	// Defaults on. Set false to leave the machine's routing to fend for itself.
	KillSwitch bool

	RefreshMargin time.Duration

	// HandshakeTimeout bounds the wait for a first handshake.
	HandshakeTimeout time.Duration

	// Monitor runs a health check for as long as a tunnel is up, and reconnects
	// when it finds the tunnel dead. Without it, a tunnel that dies on its own
	// — a wifi change, a sleeping laptop, a node restart — goes unnoticed and
	// the kill switch stays armed, which leaves the machine with no internet.
	// Defaults on; cmd/mystd sets it from -no-monitor.
	Monitor bool

	// MonitorInterval is how often the tunnel is checked.
	MonitorInterval time.Duration

	// StaleAfter is how long the tunnel may go without a handshake before it is
	// presumed dead.
	StaleAfter time.Duration

	// MaxRecoveryAttempts and RecoveryWindow bound a reconnect before it gives
	// up and returns the network.
	MaxRecoveryAttempts int
	RecoveryWindow      time.Duration

	// LocationsTTL is how long a fetched location list is served from cache.
	// Zero means DefaultLocationsTTL.
	LocationsTTL time.Duration
}

// Deps are the daemon's collaborators. Tests substitute fakes here.
type Deps struct {
	Tunnel      Tunnel
	APIFor      APIFor
	Tokens      TokenSource
	Store       auth.Store
	AcquireLock LockFunc
	KillSwitch  KillSwitch
	Logger      *slog.Logger
}

// Daemon implements ipc.Handler.
type Daemon struct {
	cfg    Config
	tun    Tunnel
	apiFor APIFor
	tokens TokenSource
	store  auth.Store
	lock   LockFunc
	ks     KillSwitch
	log    *slog.Logger

	// opMu serialises the lifecycle operations — Connect, Disconnect, Close and
	// a monitor-driven recovery. Connect is slow and touches global system
	// state; two concurrent connects would race on the interface, the config
	// file, and the account's connection.
	//
	// It is held for as long as the operation takes, which can be minutes, so
	// nothing a client waits on may take it. See mu.
	opMu sync.Mutex

	// mu guards the handful of fields below. It is never held across a network
	// call, a filesystem call, or an exec, so Status answers in microseconds
	// even while a connect or a recovery is in flight. That matters most
	// exactly when things are going wrong: `myst status` blocking for two
	// minutes during a failed reconnect is useless to the person running it.
	//
	// Lock order is opMu then mu, never the reverse.
	mu     sync.Mutex
	status ipc.Status

	// held is the wake lock for the live tunnel, or nil.
	held Lock

	// monitorCancel stops the health monitor for the current connection. Nil
	// when no monitor is running.
	monitorCancel context.CancelFunc

	// conn is the region the live tunnel serves, so a recovery reconnects to
	// the same place rather than quietly landing the user in another country.
	conn region

	// baseline is the default route the tunnel came up on. A later change means
	// the machine moved networks and the tunnel is dead.
	baseline string

	// locsMu guards locs, the cached location lists. It is deliberately not mu:
	// a cache miss costs a network round trip of up to thirty seconds, and mu
	// must never be held across anything slower than a field assignment or
	// `myst status` stops answering while the picker loads.
	//
	// Lock order is opMu, then locsMu, then mu. Nothing takes opMu while
	// holding either of the others.
	locsMu sync.Mutex
	locs   map[string]locationEntry
}

// region is a connection target, as the API understands it.
type region struct {
	country string
	city    string
	ipType  string
}

// locationEntry is a cached location list and when it was fetched.
type locationEntry struct {
	fetched time.Time
	items   []ipc.LocationCountry
}

// DefaultLocationsTTL is how long a cached location list is served before it is
// fetched again.
//
// Long, because this list is close to static — nodes come and go hourly, but
// countries and cities change on the scale of weeks. The cost of being stale is
// that a picker briefly offers a city that has just been retired, which the
// connect call then rejects with the same 2332 error any unavailable region
// gives. The cost of being short is re-downloading a megabyte every time
// someone opens a dropdown.
const DefaultLocationsTTL = 6 * time.Hour

// teardown's keepWakeLock argument, named so the call sites state their intent.
const (
	releaseWakeLock = false
	keepWakeLock    = true
)

// New returns a Daemon with defaults filled in.
func New(cfg Config, deps Deps) *Daemon {
	if cfg.BaseURL == "" {
		cfg.BaseURL = api.DefaultBaseURL
	}
	if cfg.TokenPath == "" {
		cfg.TokenPath = DefaultTokenPath
	}
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = DefaultConfigPath
	}
	if cfg.Interface == "" {
		cfg.Interface = wg.InterfaceName
	}
	if cfg.RefreshMargin == 0 {
		cfg.RefreshMargin = DefaultRefreshMargin
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if cfg.MonitorInterval == 0 {
		cfg.MonitorInterval = DefaultMonitorInterval
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = DefaultStaleAfter
	}
	if cfg.MaxRecoveryAttempts == 0 {
		cfg.MaxRecoveryAttempts = DefaultMaxRecoveryAttempts
	}
	if cfg.RecoveryWindow == 0 {
		cfg.RecoveryWindow = DefaultRecoveryWindow
	}
	if cfg.LocationsTTL == 0 {
		cfg.LocationsTTL = DefaultLocationsTTL
	}

	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if deps.Tunnel == nil {
		deps.Tunnel = WGTunnel{}
	}
	if deps.APIFor == nil {
		base := cfg.BaseURL
		deps.APIFor = func(token string) Provisioner {
			c := api.New(token)
			c.BaseURL = base
			return c
		}
	}
	if deps.Tokens == nil {
		deps.Tokens = auth.NewExchanger(cfg.BaseURL)
	}
	if deps.Store.Path == "" {
		deps.Store = auth.Store{Path: cfg.TokenPath}
	}
	if deps.AcquireLock == nil {
		deps.AcquireLock = defaultLock
	}
	if deps.KillSwitch == nil {
		deps.KillSwitch = killswitch.Nft{}
	}

	return &Daemon{
		cfg:    cfg,
		tun:    deps.Tunnel,
		apiFor: deps.APIFor,
		tokens: deps.Tokens,
		store:  deps.Store,
		lock:   deps.AcquireLock,
		ks:     deps.KillSwitch,
		log:    deps.Logger,
		status: ipc.Status{State: ipc.StateDisconnected},
		locs:   map[string]locationEntry{},
	}
}

// defaultLock acquires a real logind inhibitor.
func defaultLock(ctx context.Context, why string) (Lock, error) {
	return wakelock.Acquire(ctx, why)
}

// Connect brings the tunnel up, or returns the current status if it is already
// up in the requested region.
func (d *Daemon) Connect(ctx context.Context, t ipc.Target) (*ipc.Status, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()

	// Any monitor belongs to the connection being replaced. Stopping it here
	// rather than in teardown keeps the monitor from cancelling itself: a
	// recovery runs inside the monitor goroutine and calls teardown too.
	d.stopMonitor()

	if st := d.snapshot(); st.State == ipc.StateConnected {
		if sameTarget(t, st) {
			return d.snapshot(), nil // already there; connecting is idempotent
		}
		// A different region was asked for, so the current tunnel has to go.
		if err := d.teardown(ctx, releaseWakeLock); err != nil {
			d.log.Warn("teardown before reconnect failed", "err", err)
		}
	}

	d.setState(ipc.StateConnecting, "")

	st, err := d.bringUp(ctx, t)
	if err != nil {
		// Roll back whatever did come up. A half-connected daemon is worse than
		// a disconnected one: the interface may be carrying traffic with no
		// wake lock, no accounting, and no way to disconnect it.
		if cerr := d.teardown(ctx, releaseWakeLock); cerr != nil {
			d.log.Warn("rollback after failed connect", "err", cerr)
		}
		d.publish(ipc.Status{
			State:     ipc.StateError,
			LastError: err.Error(),
		})
		return d.snapshot(), err
	}

	d.publish(*st)
	d.noteTarget(t, st)
	d.noteBaseline(ctx)
	d.startMonitor()

	d.log.Info("tunnel up",
		"interface", st.Interface,
		"exit_ip", st.ExitIP,
		"country", st.Country,
		"city", st.City,
		"ipv6", st.IPv6Routed,
		"wake_lock", st.WakeLock,
	)
	return d.snapshot(), nil
}

// sameTarget reports whether the live tunnel already serves what t asks for.
//
// An empty field in t is a preference not expressed, not a demand for a
// region with no country — the API reads it as "you choose". So asking for
// "any city in GB" while sitting in GB/London is a match, and does not cost the
// user a needless reconnect.
//
// Comparing the ip type as well as the country fixes a quieter bug: switching
// between residential and hosting in the same country used to be silently
// ignored, leaving the user on the ip type they were trying to leave.
func sameTarget(t ipc.Target, st *ipc.Status) bool {
	matches := func(requested, current string) bool {
		return requested == "" || strings.EqualFold(requested, current)
	}
	return matches(t.Country, st.Country) &&
		matches(t.City, st.City) &&
		matches(t.IPType, st.IPType)
}

// noServerError explains a 2332 in terms the user can act on.
//
// The API returns "no available instance for filters" for a region that is
// genuinely out of nodes *and* for a filter that named nothing at all — a city
// slug in the wrong case, or an ip_type with no capacity in that country. So
// this names the most specific part of the target, which is the likeliest
// culprit, without claiming to know which of those it was.
func noServerError(t ipc.Target) error {
	hint := " — try another region, or connect without a country to let the client pick the fastest"
	if strings.EqualFold(t.IPType, "hosting") {
		// The data centre pool is a third the size of the residential one — 49
		// countries against 106 — so an exhausted city here is ordinary rather
		// than exceptional, and saying so beats "try again shortly".
		hint = " — the data centre pool is much smaller than the residential one, so try another city or switch to residential"
	}

	switch {
	case t.City != "" && t.Country != "":
		return fmt.Errorf("no servers are available in %q (%s) right now%s", t.City, t.Country, hint)
	case t.Country != "":
		return fmt.Errorf("no servers are available in %q at the moment%s", t.Country, hint)
	default:
		return errors.New("no servers are available at the moment — try again shortly")
	}
}

// Locations lists every country and city the account can exit through, from the
// cache when it is warm.
//
// Like Countries, and for the same reason, it does not take opMu: filling a
// region picker has nothing to do with the tunnel, and blocking it behind a
// two-minute connect would leave the picker empty for exactly as long as the
// user is waiting to connect.
func (d *Daemon) Locations(ctx context.Context, ipType string) ([]ipc.LocationCountry, error) {
	key := normalizeIPType(ipType)

	d.locsMu.Lock()
	defer d.locsMu.Unlock()

	if entry, ok := d.locs[key]; ok {
		if age := time.Since(entry.fetched); age < d.cfg.LocationsTTL {
			d.log.Debug("serving cached locations", "ip_type", key, "age", age.Round(time.Second), "countries", len(entry.items))
			return entry.items, nil
		}
	}

	items, err := d.fetchLocations(ctx, ipType)
	if err != nil {
		// A stale list beats no list. The alternative is a picker that empties
		// itself whenever the API has a bad minute, which is a worse failure
		// than offering a city that was retired this morning — and it is the
		// connect call, not the picker, that has the final say on whether a
		// city still exists.
		if entry, ok := d.locs[key]; ok {
			d.log.Warn("could not refresh locations; serving the cached list",
				"ip_type", key, "age", time.Since(entry.fetched).Round(time.Minute), "err", err)
			return entry.items, nil
		}
		return nil, err
	}

	d.locs[key] = locationEntry{fetched: time.Now(), items: items}
	d.log.Info("locations fetched", "ip_type", key, "countries", len(items))
	return items, nil
}

// fetchLocations retrieves and trims the location list. Caller holds locsMu.
func (d *Daemon) fetchLocations(ctx context.Context, ipType string) ([]ipc.LocationCountry, error) {
	tok, err := d.tokens.EnsureValid(ctx, d.store, d.cfg.RefreshMargin)
	if err != nil {
		return nil, err
	}
	raw, err := d.apiFor(tok.AccessToken).Locations(ctx, ipType)
	if err != nil {
		return nil, fmt.Errorf("list locations: %w", err)
	}

	// Project down to what a picker can draw. The API's record carries fourteen
	// translations, coordinates, plan lists and node counts per entry; none of
	// it is showable and all of it is multiplied by 1720 cities.
	out := make([]ipc.LocationCountry, 0, len(raw))
	for _, country := range raw {
		if country.Country == "" {
			continue // nothing to connect to and nothing to label
		}
		c := ipc.LocationCountry{
			Code: country.Country,
			Name: api.EnglishName(country.Translations, country.Country),
		}
		for _, city := range country.Cities {
			if city.City == "" {
				continue
			}
			c.Cities = append(c.Cities, ipc.LocationCity{
				Slug: city.City,
				Name: api.EnglishName(city.Translations, city.City),
			})
		}
		out = append(out, c)
	}
	return out, nil
}

// normalizeIPType maps an empty or oddly-cased ip type onto the key the cache
// and the API both use.
//
// Empty means residential to the API, so it has to mean residential here too —
// otherwise asking for "" and for "residential" would fetch the same megabyte
// twice and cache it under two names.
func normalizeIPType(ipType string) string {
	t := strings.ToLower(strings.TrimSpace(ipType))
	if t == "" {
		return "residential"
	}
	return t
}

// forgetLocations drops the cached location lists.
//
// Called when the account changes: the lists are per-account, so leaving them
// in place would offer the previous account's cities to the new one.
func (d *Daemon) forgetLocations() {
	d.locsMu.Lock()
	defer d.locsMu.Unlock()
	d.locs = map[string]locationEntry{}
}

// Countries lists the country codes the account can connect to.
//
// The full list lives behind an authenticated endpoint, so it can only be
// fetched with the token — which is the daemon's. Serving it over the control
// socket is what lets the window and the tray offer every country without
// holding a credential of their own.
//
// It deliberately does not take opMu: reading a list has nothing to do with
// the tunnel, and blocking it behind a two-minute connect would leave the
// region picker empty for as long as the user is waiting to connect.
func (d *Daemon) Countries(ctx context.Context) ([]string, error) {
	tok, err := d.tokens.EnsureValid(ctx, d.store, d.cfg.RefreshMargin)
	if err != nil {
		return nil, err
	}
	cfg, err := d.apiFor(tok.AccessToken).Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("list countries: %w", err)
	}
	return cfg.Countries, nil
}

// bringUp performs the full connect sequence. Caller holds opMu.
func (d *Daemon) bringUp(ctx context.Context, t ipc.Target) (*ipc.Status, error) {
	tok, err := d.tokens.EnsureValid(ctx, d.store, d.cfg.RefreshMargin)
	if err != nil {
		return nil, err
	}
	client := d.apiFor(tok.AccessToken)

	// Generate the keypair here, so the private key never leaves the machine.
	kp, err := wg.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	resp, err := client.Connect(ctx, api.WireguardConnectRequest{
		PublicKey: kp.PublicKey,
		IPType:    t.IPType,
		Country:   t.Country,
		City:      t.City,
	})
	if err != nil {
		// A country with no node free is not a server fault, though the API
		// reports it as one. Saying so plainly is the difference between a user
		// who picks another region and one who concludes the client is broken.
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.IsNoServerInRegion() {
			return nil, noServerError(t)
		}
		return nil, fmt.Errorf("provision tunnel: %w", err)
	}
	if resp.WGConfig == "" {
		return nil, errors.New("server returned an empty wg_config")
	}
	if resp.LimitExceeded {
		// Measured to be advisory: it appears on every connect, even with slots
		// free, and does not block. Worth logging, not worth failing on.
		d.log.Info("server reported limit_exceeded; known to be advisory, continuing")
	}

	conf, err := wg.Assemble(resp.WGConfig, kp)
	if err != nil {
		return nil, fmt.Errorf("assemble config: %w", err)
	}
	if d.cfg.IPv6 {
		conf = wg.EnsureIPv6Route(conf)
	}
	conf = wg.EnsureKeepalive(conf)
	if err := wg.Validate(conf); err != nil {
		return nil, fmt.Errorf("assembled config is unusable: %w", err)
	}
	if err := wg.WriteConfig(d.cfg.ConfigPath, conf); err != nil {
		return nil, err
	}

	// Arm the filter before the interface exists, so there is no window in
	// which traffic could escape in the clear. The endpoint has to be resolved
	// first, while DNS is still reachable.
	killSwitchArmed := false
	if d.cfg.KillSwitch {
		eps, err := d.handshakeEndpoints(ctx, conf)
		if err != nil {
			return nil, err
		}
		if err := d.ks.Install(ctx, killswitch.Spec{Interface: d.cfg.Interface, Endpoints: eps}); err != nil {
			return nil, fmt.Errorf("arm the kill switch: %w", err)
		}
		killSwitchArmed = true
	}

	if err := d.tun.Up(ctx, d.cfg.ConfigPath); err != nil {
		return nil, fmt.Errorf("bring up %s: %w", d.cfg.Interface, err)
	}

	// Hold the machine awake before anything else can go wrong, so a suspend
	// cannot land in the window between "up" and "protected".
	wakeLock := false
	if d.cfg.WakeLock {
		lk, err := d.lock(ctx, "Mysterium VPN tunnel is up")
		if err != nil {
			// Not fatal: the tunnel works, it is just vulnerable to suspend.
			d.log.Warn("could not take a wake lock; the machine may sleep and drop the tunnel", "err", err)
		} else {
			d.setHeld(lk)
			wakeLock = true
		}
	}

	if _, err := d.waitForHandshake(ctx); err != nil {
		return nil, err
	}

	// Confirm traffic actually egresses where the API said it would. A mismatch
	// is not fatal — the API's exit_ip is advisory and the node may differ — but
	// it is exactly the kind of thing that should not pass silently.
	exitIP := resp.ExitIP
	if observed, err := d.tun.ExitIP(ctx); err != nil {
		d.log.Warn("could not verify the exit IP", "err", err)
	} else {
		if resp.ExitIP != "" && observed != resp.ExitIP {
			d.log.Warn("exit IP differs from the one the API reported",
				"api", resp.ExitIP, "observed", observed)
		}
		exitIP = observed
	}

	return &ipc.Status{
		State:      ipc.StateConnected,
		Since:      time.Now(),
		ExitIP:     exitIP,
		Country:    resp.Country,
		City:       resp.City,
		IPType:     resp.IPType,
		Interface:  d.cfg.Interface,
		IPv6Routed: wg.RoutesIPv6(conf),
		WakeLock:   wakeLock,
		KillSwitch: killSwitchArmed,
	}, nil
}

// handshakeEndpoints returns the destinations that must stay reachable while
// the kill switch is armed.
//
// Getting this wrong is self-defeating in a specific way: a filter that blocks
// the WireGuard endpoint prevents the tunnel from ever re-establishing, so the
// machine would sit offline until someone found the escape hatch.
func (d *Daemon) handshakeEndpoints(ctx context.Context, conf string) ([]killswitch.Endpoint, error) {
	host, port, err := wg.Endpoint(conf)
	if err != nil {
		return nil, fmt.Errorf("cannot arm the kill switch: %w", err)
	}

	// Mysterium hands back a bare IP, so this is normally a no-op. Resolving
	// anyway means a hostname endpoint still produces a correct filter rather
	// than one that silently blocks the handshake.
	if ip := net.ParseIP(host); ip != nil {
		return []killswitch.Endpoint{{IP: ip.String(), Port: port}}, nil
	}

	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("cannot arm the kill switch: resolve endpoint %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cannot arm the kill switch: endpoint %q resolved to no addresses", host)
	}

	eps := make([]killswitch.Endpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, killswitch.Endpoint{IP: a.Unmap().String(), Port: port})
	}
	return eps, nil
}

// waitForHandshake waits for positive evidence that the tunnel works.
//
// An interface that is up but has never handshaked passes every other check we
// make and still carries no traffic, so it is worth spending a few seconds to
// rule out rather than reporting a success that is not one.
func (d *Daemon) waitForHandshake(ctx context.Context) (time.Time, error) {
	deadline := time.Now().Add(d.cfg.HandshakeTimeout)
	var lastErr error

	for {
		hs, err := d.tun.Handshake(ctx)
		switch {
		case err != nil:
			lastErr = err
		case !hs.IsZero():
			return hs, nil
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return time.Time{}, fmt.Errorf("no handshake within %s: %w", d.cfg.HandshakeTimeout, lastErr)
			}
			return time.Time{}, fmt.Errorf(
				"no handshake within %s: %s is up but the peer is not responding", d.cfg.HandshakeTimeout, d.cfg.Interface)
		}

		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// --- health monitor ---------------------------------------------------------

// startMonitor launches the health monitor for a tunnel that has just come up.
// Caller holds opMu.
//
// The monitor runs on its own context, deliberately not the caller's. Connect
// is invoked from an IPC call with a ninety-second budget, and a monitor that
// died with the request would stop watching after a minute and a half — which
// is precisely when a user has walked away and left the tunnel running.
func (d *Daemon) startMonitor() {
	if !d.cfg.Monitor {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())

	d.mu.Lock()
	d.monitorCancel = cancel
	d.mu.Unlock()

	go d.monitor(ctx)
}

// stopMonitor stops the health monitor, if one is running. Caller holds opMu.
// Safe to call when none is running, which is the common case.
func (d *Daemon) stopMonitor() {
	d.mu.Lock()
	cancel := d.monitorCancel
	d.monitorCancel = nil
	d.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// monitor watches a live tunnel and repairs it, until its context is cancelled
// by a disconnect, a new connect, or shutdown.
func (d *Daemon) monitor(ctx context.Context) {
	t := time.NewTicker(d.cfg.MonitorInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		reason, dead := d.tunnelDead(ctx)
		if !dead {
			continue
		}
		// A disconnect may have landed while this tick was in flight, in which
		// case there is no longer a tunnel to repair.
		if ctx.Err() != nil {
			return
		}
		// Claim before doing any work. A status poll that noticed the same dead
		// tunnel a moment ago will have claimed it already, and in that case
		// there is nothing for this tick to do but keep watching.
		if !d.claimDeadTunnel(reason) {
			// Someone else owns the recovery. If the state they left behind is
			// terminal then the tunnel is gone for good and there is nothing
			// left to watch.
			switch d.snapshot().State {
			case ipc.StateDisconnected, ipc.StateError:
				return
			}
			continue
		}
		if keepWatching := d.rebuild(ctx, reason); !keepWatching {
			return
		}
	}
}

// claimDeadTunnel moves a live connection into the reconnecting state and
// reports whether this caller is the one that claimed it.
//
// Only "connected" can be claimed, which is what stops two callers starting a
// recovery for the same dead tunnel. That is not hypothetical: a user running
// `myst status` in a loop would otherwise launch a recovery per poll, and each
// one would tear down whatever the last had just built.
func (d *Daemon) claimDeadTunnel(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.status.State != ipc.StateConnected {
		return false
	}
	d.status.State = ipc.StateReconnecting
	d.status.LastError = reason
	return true
}

// recoverDetached rebuilds a tunnel that a status check found already dead.
//
// A status check cannot do the work itself — it has to answer in microseconds,
// and a recovery can take two minutes — so it hands off to this. Doing it here
// rather than waiting for the monitor matters when the monitor is disabled:
// that is a supported configuration, and it must not become the one where a
// vanished interface leaves the machine filtered with nothing behind it.
//
// The context is fresh rather than the caller's, because the caller's is
// cancelled the moment it answers.
func (d *Daemon) recoverDetached(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.RecoveryWindow+time.Minute)
	defer cancel()

	d.rebuild(ctx, reason)
}

// tunnelDead reports whether the live tunnel has stopped working, and why.
//
// Three signals, cheapest and most immediate first. The route check is what
// catches the failure that motivated this monitor — a machine moving between
// networks — within a single interval. The handshake check is the backstop for
// everything else, including a node that goes away at the far end.
func (d *Daemon) tunnelDead(ctx context.Context) (string, bool) {
	// A WireGuard interface is virtual: it stays UP when the network beneath it
	// disappears, so this catches only the interface being removed outright.
	if !d.tun.IsUp(ctx) {
		return fmt.Sprintf("%s is gone", d.cfg.Interface), true
	}

	// The default route changes the instant the network does, which is well
	// before the handshake has had time to look stale.
	if route, err := d.tun.DefaultRoute(ctx); err != nil {
		// Failing to read the route is not evidence that the tunnel is down.
		// Fall through to the handshake check rather than flapping on it.
		d.log.Debug("cannot read the default route", "err", err)
	} else if want := d.routeBaseline(); want != "" && route.String() != want {
		return fmt.Sprintf("the default route changed from %s to %s", want, route), true
	}

	hs, err := d.tun.Handshake(ctx)
	switch {
	case err != nil:
		return fmt.Sprintf("cannot read the handshake time: %v", err), true
	case hs.IsZero():
		// Connect waits for a handshake before reporting success, so a tunnel
		// that has never handshaked lost its peer after we last looked.
		return "the peer has stopped responding", true
	case time.Since(hs) > d.cfg.StaleAfter:
		return fmt.Sprintf("no handshake for %s", time.Since(hs).Round(time.Second)), true
	}
	return "", false
}

// recover repairs a tunnel the monitor found dead, and reports whether the
// monitor should keep watching.
//
// Reconnect first, then fall back — the behaviour chosen when this was
// designed. Retries are bounded by both attempt count and wall-clock time, and
// when the budget runs out everything comes down, the kill switch included, and
// the failure is reported. That fall-back is the guarantee that a dead tunnel
// can never leave the machine without internet.
// rebuild replaces a tunnel that has already been claimed as dead, and reports
// whether the monitor should keep watching afterwards.
//
// The caller must have claimed it through claimDeadTunnel. That claim, not this
// function, is what guarantees a single recovery is in flight — which is why
// there is no "is it really dead?" test here: by the time anyone gets this far
// the answer is yes, and asking again would only open a window for a second
// caller to slip in behind.
func (d *Daemon) rebuild(ctx context.Context, reason string) bool {
	d.opMu.Lock()
	defer d.opMu.Unlock()

	if ctx.Err() != nil {
		return false
	}

	// The claim happened before this took opMu, so the world may have moved on
	// while it queued: the user may have disconnected, or connected somewhere
	// else. Re-check what we are actually holding.
	switch d.snapshot().State {
	case ipc.StateReconnecting:
		// Ours to finish.
	case ipc.StateConnected:
		// Someone rebuilt it while this waited. Nothing to do, keep watching.
		return true
	default:
		// Disconnected or already given up. Either way there is no tunnel to
		// watch any more.
		return false
	}

	d.log.Warn("the tunnel has stopped working; reconnecting", "reason", reason)

	target := d.target()
	deadline := time.Now().Add(d.cfg.RecoveryWindow)
	var lastErr error

	for attempt := 1; attempt <= d.cfg.MaxRecoveryAttempts; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		if time.Now().After(deadline) {
			lastErr = fmt.Errorf("the %s recovery window expired", d.cfg.RecoveryWindow)
			break
		}

		d.log.Info("recovery attempt", "attempt", attempt, "of", d.cfg.MaxRecoveryAttempts)

		// Clear the dead tunnel's remains before building a new one, so the
		// interface, the account's connection and the filter all start clean.
		// The wake lock stays held: letting the machine suspend here would
		// strand the recovery half-done.
		if err := d.teardown(ctx, keepWakeLock); err != nil {
			d.log.Warn("could not fully clear the dead tunnel", "err", err)
		}

		st, err := d.bringUp(ctx, ipc.Target{
			Country: target.country,
			City:    target.city,
			IPType:  target.ipType,
		})
		if err == nil {
			d.publish(*st)
			d.noteBaseline(ctx)
			d.log.Info("tunnel recovered",
				"attempt", attempt, "exit_ip", st.ExitIP, "country", st.Country)
			return true
		}

		lastErr = err
		d.log.Warn("recovery attempt failed", "attempt", attempt, "err", err)
	}

	// Out of budget. Hand the network back rather than leaving the machine
	// filtered with nothing to carry its traffic.
	d.log.Error("could not restore the tunnel; returning the network", "err", lastErr)

	msg := fmt.Sprintf(
		"the tunnel dropped (%s) and could not be restored after %d attempts: %v. "+
			"The kill switch has been disarmed, so the network works again, but traffic is no longer going through Mysterium.",
		reason, d.cfg.MaxRecoveryAttempts, lastErr)
	if err := d.teardown(ctx, releaseWakeLock); err != nil {
		msg += fmt.Sprintf(" Teardown also reported: %v", err)
	}
	d.publish(ipc.Status{State: ipc.StateError, LastError: msg})
	return false
}

// Disconnect tears the tunnel down.
func (d *Daemon) Disconnect(ctx context.Context) (*ipc.Status, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	return d.disconnectLocked(ctx)
}

// disconnectLocked is Disconnect without the lock, for callers that already
// hold opMu. Caller holds opMu.
func (d *Daemon) disconnectLocked(ctx context.Context) (*ipc.Status, error) {
	d.stopMonitor()

	err := d.teardown(ctx, releaseWakeLock)
	d.publish(ipc.Status{State: ipc.StateDisconnected})
	if err != nil {
		st := ipc.Status{State: ipc.StateError, LastError: err.Error()}
		d.publish(st)
		return d.snapshot(), err
	}
	d.log.Info("tunnel down")
	return d.snapshot(), nil
}

// Login replaces the stored credentials.
//
// The credentials live in a root-only file under the daemon's state directory,
// so this is the only unprivileged route to writing them. Nothing is cached:
// every operation loads the token from disk, so a new account takes effect on
// the next connect without restarting the daemon.
func (d *Daemon) Login(ctx context.Context, credentials []byte) (*ipc.Status, error) {
	var tok auth.Token
	if err := json.Unmarshal(credentials, &tok); err != nil {
		return nil, fmt.Errorf("that is not a token set: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("that token set has no access_token")
	}
	if tok.ObtainedAt.IsZero() {
		tok.ObtainedAt = time.Now()
	}

	d.opMu.Lock()
	defer d.opMu.Unlock()

	// Close any tunnel first, and do it on the old credentials.
	//
	// Order matters. The disconnect call tells the API to release the
	// connection, and it has to name the account that opened it. Overwriting
	// the token first would ask the *new* account to release a connection it
	// never made, so the old one would stay registered until it timed out on
	// its own — a leak the user cannot see and did not ask for.
	if st := d.snapshot(); st.State == ipc.StateConnected || st.State == ipc.StateReconnecting {
		if _, err := d.disconnectLocked(ctx); err != nil {
			return d.snapshot(), fmt.Errorf("a tunnel is up and could not be closed before switching accounts: %w", err)
		}
	}

	if err := d.store.Save(&tok); err != nil {
		return nil, fmt.Errorf("store credentials: %w", err)
	}
	// The location lists are per-account. Keeping them would offer the previous
	// account's cities to the new one, which looks like a stale picker rather
	// than a credential change.
	d.forgetLocations()
	d.log.Info("credentials replaced")
	return d.snapshot(), nil
}

// Logout deletes the stored credentials, closing any tunnel first.
func (d *Daemon) Logout(ctx context.Context) (*ipc.Status, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()

	// Same ordering as Login: release the account's connection while the token
	// that opened it is still on disk.
	if st := d.snapshot(); st.State == ipc.StateConnected || st.State == ipc.StateReconnecting {
		if _, err := d.disconnectLocked(ctx); err != nil {
			return d.snapshot(), fmt.Errorf("a tunnel is up and could not be closed: %w", err)
		}
	}

	if err := d.store.Delete(); err != nil {
		return nil, err
	}
	d.forgetLocations()
	d.log.Info("credentials removed")
	return d.snapshot(), nil
}

// teardown undoes a connection. Caller holds opMu.
//
// Every step runs even if an earlier one fails, and the failures are joined:
// stopping at the first error would leave the account holding a connection, or
// a private key on disk, purely because the interface happened to be stubborn.
//
// It deliberately does not touch the health monitor, because a recovery calls
// it from inside the monitor goroutine and stopping the monitor there would
// cancel the recovery itself. Connect, Disconnect and Close stop it instead.
func (d *Daemon) teardown(ctx context.Context, wakeLock bool) error {
	var errs []error

	if d.tun.IsUp(ctx) {
		if err := d.tun.Down(ctx); err != nil {
			errs = append(errs, fmt.Errorf("bring %s down: %w", d.cfg.Interface, err))
		}
	}

	// A recovery passes keepWakeLock: the machine must not be free to suspend
	// in the middle of a reconnect, and holding the lock a few seconds longer
	// costs nothing.
	if wakeLock == releaseWakeLock {
		if lk := d.takeHeld(); lk != nil {
			lk.Release()
		}
	}

	// Disarm only after the interface is down. Removing the filter first would
	// open a window in which traffic could leave in the clear while routes
	// still pointed at the tunnel.
	//
	// Called unconditionally, even when the kill switch is disabled: it also
	// clears a table a previous run left behind.
	if err := d.ks.Remove(ctx); err != nil {
		errs = append(errs, fmt.Errorf("disarm the kill switch: %w", err))
	}

	// Release server-side even if the local half failed, so the account does
	// not accumulate connections it believes are still live.
	if tok, err := d.store.Load(); err == nil && tok.AccessToken != "" {
		if err := d.apiFor(tok.AccessToken).Disconnect(ctx); err != nil {
			errs = append(errs, fmt.Errorf("release server-side connection: %w", err))
		}
	}

	// The config file holds a private key; it should not outlive the tunnel.
	if err := os.Remove(d.cfg.ConfigPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove config: %w", err))
	}

	return errors.Join(errs...)
}

// Status reports the current state, reconciled against reality.
func (d *Daemon) Status(ctx context.Context) (*ipc.Status, error) {
	// Cheap check first: reading the published state takes the short lock, so
	// the common case costs nothing and cannot be blocked by a connect.
	if st := d.snapshot(); st.State == ipc.StateConnected && !d.tun.IsUp(ctx) {
		// The interface can vanish without us — a manual `wg-quick down`, a
		// module unload, a killed process. Reporting "connected" after that
		// would be a lie, and worse, the kill switch would stay armed with no
		// tunnel behind it.
		reason := fmt.Sprintf("%s disappeared — the tunnel dropped outside the daemon", d.cfg.Interface)
		if d.claimDeadTunnel(reason) {
			d.log.Warn("interface disappeared while we believed it was up", "interface", d.cfg.Interface)
			// Repair, do not give up. An earlier version tore the connection
			// down here, on the theory that a tunnel which died outside the
			// daemon was beyond saving — but this is the *safety net*, and the
			// monitor that would otherwise rebuild it ticks on a 10s timer, so
			// a user polling status during a drop would destroy the recovery
			// seconds before it started. Hand off instead, and let rebuild
			// decide whether to reconnect or fall back.
			go d.recoverDetached(reason)
		}
	}

	return d.snapshot(), nil
}

// Close tears everything down. Intended for graceful shutdown.
func (d *Daemon) Close(ctx context.Context) error {
	d.opMu.Lock()
	defer d.opMu.Unlock()

	d.stopMonitor()
	return d.teardown(ctx, releaseWakeLock)
}

// --- status accessors -------------------------------------------------------
//
// Every one of these takes mu and releases it immediately. They exist so no
// caller has to remember which of the two locks guards which field.

func (d *Daemon) snapshot() *ipc.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.status
	return &st
}

// publish replaces the whole published status.
func (d *Daemon) publish(st ipc.Status) {
	d.mu.Lock()
	d.status = st
	d.mu.Unlock()
}

// setState moves the state machine without disturbing the rest of the status.
func (d *Daemon) setState(state ipc.State, lastErr string) {
	d.mu.Lock()
	d.status.State = state
	d.status.LastError = lastErr
	d.mu.Unlock()
}

func (d *Daemon) setHeld(lk Lock) {
	d.mu.Lock()
	d.held = lk
	d.mu.Unlock()
}

// takeHeld removes and returns the wake lock, so the caller can release it
// without holding mu across whatever Release does.
func (d *Daemon) takeHeld() Lock {
	d.mu.Lock()
	defer d.mu.Unlock()
	lk := d.held
	d.held = nil
	return lk
}

// noteTarget records the region the live tunnel serves.
// noteTarget records where the live tunnel serves, so a recovery rebuilds the
// same place rather than quietly landing the user somewhere they did not ask
// for.
//
// The country and ip type come from the status, because the API canonicalises
// them — the country is folded to uppercase ISO, and an absent ip type comes
// back as "residential". Feeding a differently-cased code back would fail the
// filter outright.
//
// The city comes from what was *asked for*, not what was assigned, and the
// difference is deliberate. When the user named a city, that choice has to
// survive a recovery. When they asked only for a country, the city the API
// picked is an implementation detail — pinning the recovery to it would narrow
// a reconnect that has every reason to be broad, and could fail for want of one
// city when the country has others free.
func (d *Daemon) noteTarget(t ipc.Target, st *ipc.Status) {
	d.mu.Lock()
	d.conn = region{country: st.Country, city: t.City, ipType: st.IPType}
	d.mu.Unlock()
}

func (d *Daemon) target() region {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn
}

// noteBaseline records the default route the tunnel came up on, so the monitor
// can tell later that the machine has moved networks.
func (d *Daemon) noteBaseline(ctx context.Context) {
	route, err := d.tun.DefaultRoute(ctx)
	if err != nil {
		// Without a baseline the route check simply cannot fire; the handshake
		// check still covers the tunnel. Not worth failing a connect over.
		d.log.Debug("cannot record the default route for the health monitor", "err", err)
		return
	}
	d.mu.Lock()
	d.baseline = route.String()
	d.mu.Unlock()
}

func (d *Daemon) routeBaseline() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseline
}
