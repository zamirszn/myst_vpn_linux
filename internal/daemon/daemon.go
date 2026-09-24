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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
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
}

// Provisioner is the control plane, bound to one access token.
type Provisioner interface {
	Connect(ctx context.Context, req api.WireguardConnectRequest) (*api.WireguardConnectResponse, error)
	Disconnect(ctx context.Context) error
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

// Defaults applied where Config leaves a field zero.
const (
	DefaultTokenPath     = "/var/lib/mystd/token.json"
	DefaultConfigPath    = "/etc/wireguard/myst0.conf"
	DefaultRefreshMargin = 24 * time.Hour

	// DefaultHandshakeTimeout bounds how long we wait for positive evidence that
	// the tunnel works before declaring success.
	DefaultHandshakeTimeout = 10 * time.Second
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

	// mu serialises the state machine. Connect is slow and touches global
	// system state; two concurrent connects would race on the interface, the
	// config file, and the account's connection.
	mu     sync.Mutex
	status ipc.Status
	held   Lock
}

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
	}
}

// defaultLock acquires a real logind inhibitor.
func defaultLock(ctx context.Context, why string) (Lock, error) {
	return wakelock.Acquire(ctx, why)
}

// Connect brings the tunnel up, or returns the current status if it is already
// up in the requested region.
func (d *Daemon) Connect(ctx context.Context, country, ipType string) (*ipc.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.status.State == ipc.StateConnected {
		if country == "" || country == d.status.Country {
			return d.snapshot(), nil // already there; connecting is idempotent
		}
		// A different region was asked for, so the current tunnel has to go.
		if err := d.teardown(ctx); err != nil {
			d.log.Warn("teardown before reconnect failed", "err", err)
		}
	}

	d.status.State = ipc.StateConnecting
	d.status.LastError = ""

	st, err := d.bringUp(ctx, country, ipType)
	if err != nil {
		// Roll back whatever did come up. A half-connected daemon is worse than
		// a disconnected one: the interface may be carrying traffic with no
		// wake lock, no accounting, and no way to disconnect it.
		if cerr := d.teardown(ctx); cerr != nil {
			d.log.Warn("rollback after failed connect", "err", cerr)
		}
		d.status = ipc.Status{
			State:     ipc.StateError,
			LastError: err.Error(),
		}
		return d.snapshot(), err
	}

	d.status = *st
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

// bringUp performs the full connect sequence. Caller holds mu.
func (d *Daemon) bringUp(ctx context.Context, country, ipType string) (*ipc.Status, error) {
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
		IPType:    ipType,
		Country:   country,
	})
	if err != nil {
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
			d.held = lk
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

// Disconnect tears the tunnel down.
func (d *Daemon) Disconnect(ctx context.Context) (*ipc.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	err := d.teardown(ctx)
	d.status = ipc.Status{State: ipc.StateDisconnected}
	if err != nil {
		d.status.State = ipc.StateError
		d.status.LastError = err.Error()
		return d.snapshot(), err
	}
	d.log.Info("tunnel down")
	return d.snapshot(), nil
}

// teardown undoes a connection. Caller holds mu.
//
// Every step runs even if an earlier one fails, and the failures are joined:
// stopping at the first error would leave the account holding a connection, or
// a private key on disk, purely because the interface happened to be stubborn.
func (d *Daemon) teardown(ctx context.Context) error {
	var errs []error

	if d.tun.IsUp(ctx) {
		if err := d.tun.Down(ctx); err != nil {
			errs = append(errs, fmt.Errorf("bring %s down: %w", d.cfg.Interface, err))
		}
	}

	if d.held != nil {
		d.held.Release()
		d.held = nil
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
	d.mu.Lock()
	defer d.mu.Unlock()

	// The interface can vanish without us — a manual `wg-quick down`, a module
	// unload, a killed process. Reporting "connected" after that would be a lie,
	// and the wake lock would be held for a tunnel that no longer exists.
	if d.status.State == ipc.StateConnected && !d.tun.IsUp(ctx) {
		d.log.Warn("interface disappeared while we believed it was up", "interface", d.cfg.Interface)
		if d.held != nil {
			d.held.Release()
			d.held = nil
		}
		d.status = ipc.Status{
			State:     ipc.StateError,
			LastError: fmt.Sprintf("%s disappeared — the tunnel dropped outside the daemon", d.cfg.Interface),
		}
	}

	return d.snapshot(), nil
}

// Close tears everything down. Intended for graceful shutdown.
func (d *Daemon) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.teardown(ctx)
}

func (d *Daemon) snapshot() *ipc.Status {
	st := d.status
	return &st
}
