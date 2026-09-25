package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// D-Bus names and paths this applet owns.
const (
	sniInterface = "org.kde.StatusNotifierItem"
	sniPath      = dbus.ObjectPath("/StatusNotifierItem")

	menuInterface = "com.canonical.dbusmenu"
	menuPath      = dbus.ObjectPath("/MenuBar")

	propsInterface = "org.freedesktop.DBus.Properties"
)

// pollInterval is how often the daemon is asked for its state, so the icon
// follows changes the applet did not cause — a CLI disconnect, a tunnel that
// dropped on its own, a reconnect after an error.
const pollInterval = 5 * time.Second

// daemonClient is the daemon half of the applet. An interface rather than the
// concrete client so the state machine can be driven in tests without a
// socket, a daemon, or a D-Bus connection.
type daemonClient interface {
	Status(ctx context.Context) (*status, error)
	Connect(ctx context.Context, country, ipType string) (*status, error)
	Disconnect(ctx context.Context) (*status, error)
	Countries(ctx context.Context) ([]string, error)
}

// app is the shared state behind both D-Bus objects. Every mutation goes
// through mutate, which is what keeps the icon, the tooltip, and the menu in
// step with each other instead of each caller remembering to refresh them.
type app struct {
	conn   *dbus.Conn
	client daemonClient

	// emitTo publishes a signal. Nil in tests that only care about state, and
	// nil-safe everywhere, so no caller has to check whether a bus is attached.
	emitTo func(dbus.ObjectPath, string, ...any) error

	mu sync.Mutex

	// st is the daemon's last reported state, and is only meaningful while
	// reachable is true. reachable is false when the daemon could not be
	// contacted at all, which is a different thing from a daemon that reports
	// the tunnel is down — and a stale "connected" must never be presented as
	// current just because the socket went quiet.
	st        status
	reachable bool

	// country is the region the user picked, remembered across connects.
	country string

	// choices is the country list the Region submenu offers — the account's own
	// list once the daemon has answered, and the built-in fallback until then.
	//
	// regionIDs and regionCodes are the two directions of the mapping between a
	// country and its menu id. See idForRegion for why the id belongs to the
	// country rather than to its row.
	choices      []region
	regionIDs    map[string]int32
	regionCodes  map[int32]string
	nextRegionID int32

	// choicesSeq changes whenever choices does. It is folded into the menu key
	// because the revision is what tells the panel to re-read the menu, and a
	// swapped country list does not show up in any of the state fields.
	choicesSeq int

	// haveCountries records that choices came from the daemon. Until it does,
	// every poll asks again, so an applet started before the daemon fills in as
	// soon as it can. loadingCountries keeps one request in flight at a time.
	haveCountries    bool
	loadingCountries bool

	// busy is true while a connect or disconnect is in flight. The daemon
	// serialises commands behind one mutex, so polling status during a connect
	// would block for as long as the connect takes.
	busy bool

	// Presentation, derived from the above by syncLocked.
	iconFor   string
	icon      []pixmap
	tooltip   string
	sniStatus string
	menuFor   string
	rev       uint32

	quit     chan struct{}
	quitOnce sync.Once
}

func newApp(conn *dbus.Conn, c daemonClient, country string) *app {
	a := &app{
		conn:    conn,
		client:  c,
		country: country,
		quit:    make(chan struct{}),

		// The fallback list, so the menu has regions in it before the daemon has
		// been heard from. The account's real list replaces it on the first
		// answer, and is about five times longer.
		choices:      fallbackRegions,
		regionIDs:    map[string]int32{},
		regionCodes:  map[int32]string{},
		nextRegionID: idRegionBase,
	}
	if conn != nil {
		a.emitTo = conn.Emit
	}
	a.mu.Lock()
	a.syncLocked()
	a.mu.Unlock()
	return a
}

// presentation is the part of the state the host can see. Comparing it before
// and after a mutation is how the applet decides which signals to emit — and,
// more usefully, which not to.
type presentation struct {
	iconKey   string
	tooltip   string
	sniStatus string
	rev       uint32
}

func (a *app) presentationLocked() presentation {
	return presentation{
		iconKey:   a.iconFor,
		tooltip:   a.tooltip,
		sniStatus: a.sniStatus,
		rev:       a.rev,
	}
}

// syncLocked recomputes everything derived from the state. Caller holds mu.
func (a *app) syncLocked() {
	iconKey := a.st.State
	if !a.reachable {
		iconKey = "unreachable"
	}
	if iconKey != a.iconFor {
		a.iconFor = iconKey
		a.icon = renderPixmaps(styleFor(a.st.State, a.reachable))
	}

	a.tooltip = a.tooltipLocked()

	switch {
	case !a.reachable:
		a.sniStatus = "NeedsAttention"
	case a.st.State == stateError:
		a.sniStatus = "NeedsAttention"
	default:
		a.sniStatus = "Active"
	}

	// The menu can change for reasons the state string does not capture — a new
	// exit IP, a different selected region, a country list that arrived from the
	// daemon. Hashing what the menu actually shows keeps the revision honest
	// without every caller remembering to bump it.
	menuKey := fmt.Sprintf("%t|%s|%s|%s|%s|%d",
		a.reachable, a.st.State, a.st.Country, a.st.ExitIP, a.country, a.choicesSeq)
	if menuKey != a.menuFor {
		a.menuFor = menuKey
		a.rev++
	}
}

func (a *app) tooltipLocked() string {
	switch {
	case !a.reachable:
		return "Mysterium VPN\nDaemon not reachable"
	case a.st.State == stateConnected:
		var b strings.Builder
		b.WriteString("Mysterium VPN — connected")
		if a.st.Country != "" {
			b.WriteString("\n" + regionName(a.st.Country))
			if a.st.City != "" {
				b.WriteString(" · " + titleCase(a.st.City))
			}
		}
		if a.st.ExitIP != "" {
			b.WriteString("\nExit " + a.st.ExitIP)
		}
		if !a.st.KillSwitch {
			b.WriteString("\nNo kill switch")
		}
		if !a.st.WakeLock {
			b.WriteString("\nMachine may sleep")
		}
		return b.String()
	case a.st.State == stateConnecting:
		return "Mysterium VPN — connecting…"
	case a.st.State == stateReconnecting:
		msg := "Mysterium VPN — reconnecting…"
		if a.st.LastError != "" {
			msg += "\n" + a.st.LastError
		}
		return msg
	case a.st.State == stateError:
		msg := a.st.LastError
		if msg == "" {
			msg = "the last operation failed"
		}
		return "Mysterium VPN — error\n" + msg
	default:
		return "Mysterium VPN — not connected\nClick to connect to " + regionName(a.country)
	}
}

// mutate applies fn under the lock, then emits whatever changed.
func (a *app) mutate(fn func()) {
	a.mu.Lock()
	before := a.presentationLocked()
	fn()
	a.syncLocked()
	after := a.presentationLocked()
	a.mu.Unlock()

	a.emitChanges(before, after)
}

func (a *app) emitChanges(before, after presentation) {
	if after.iconKey != before.iconKey {
		a.emitSNI("NewIcon")
	}
	if after.tooltip != before.tooltip {
		a.emitSNI("NewToolTip")
	}
	if after.sniStatus != before.sniStatus {
		a.emitSNI("NewStatus", after.sniStatus)
	}
	if after.rev != before.rev {
		a.emit(menuPath, menuInterface+".LayoutUpdated", after.rev, int32(0))
	}
}

func (a *app) emitSNI(name string, args ...any) {
	a.emit(sniPath, sniInterface+"."+name, args...)
}

func (a *app) emit(path dbus.ObjectPath, name string, args ...any) {
	if a.emitTo == nil {
		return
	}
	if err := a.emitTo(path, name, args...); err != nil {
		log.Printf("emit %s: %v", name, err)
	}
}

// loadCountries asks the daemon what countries the account can reach and puts
// them in the menu.
//
// The list is asked of the daemon rather than built in, because it is the
// account's own and it is long — about a hundred countries, against the
// eighteen worth offering when there is nobody to ask. Asking the API directly
// is not an option for a front end: it needs the access token, and the token
// belongs to the daemon.
//
// It runs off the caller's goroutine because it is a socket call, and it is
// asked for only until it succeeds — the fallback stays on screen until then,
// so an applet started before the daemon is still usable.
func (a *app) loadCountries(ctx context.Context) {
	a.mu.Lock()
	if a.haveCountries || a.loadingCountries {
		a.mu.Unlock()
		return
	}
	a.loadingCountries = true
	a.mu.Unlock()

	go func() {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		codes, err := a.client.Countries(cctx)

		a.mutate(func() {
			a.loadingCountries = false
			if err != nil || len(codes) == 0 {
				// Not worth telling the user about: they already have the fallback
				// list, and a daemon that is not answering already says so at the
				// top of the menu and in the icon.
				return
			}
			list := fromCodes(codes)
			if len(list) == 0 {
				return
			}
			a.haveCountries = true
			a.choices = list
			a.choicesSeq++
		})
	}()
}

// refresh asks the daemon for its state and folds the answer in.
func (a *app) refresh(ctx context.Context) {
	a.mu.Lock()
	busy := a.busy
	a.mu.Unlock()
	if busy {
		// The daemon holds one lock across a connect, so a status call now
		// would simply queue behind it and time out — which would look exactly
		// like the daemon having died.
		return
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	st, err := a.client.Status(cctx)

	a.mutate(func() {
		if st == nil {
			// No answer at all: the socket is gone or the daemon is wedged.
			// Say so rather than leaving the last known state on screen.
			a.reachable = false
			if err != nil {
				a.st.LastError = err.Error()
			}
			return
		}
		a.reachable = true
		a.st = *st
	})
}

// doConnect asks the daemon to bring the tunnel up. The call can take a minute,
// so it runs off the D-Bus handler goroutine entirely.
func (a *app) doConnect(country string) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return
	}
	a.busy = true
	if country != "" {
		a.country = country
	}
	target := a.country
	a.mu.Unlock()

	a.mutate(func() {
		a.st = status{State: stateConnecting, Country: target}
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		st, err := a.client.Connect(ctx, target, "")
		log.Printf("connect %s: err=%v", target, err)

		a.mutate(func() {
			a.busy = false
			a.reachable = st != nil
			switch {
			case st != nil:
				a.st = *st
			case err != nil:
				// Nothing came back, so there is no daemon state to show.
				a.st = status{State: stateError, LastError: err.Error()}
			}
		})
	}()
}

func (a *app) doDisconnect() {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return
	}
	a.busy = true
	a.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		st, err := a.client.Disconnect(ctx)
		log.Printf("disconnect: err=%v", err)

		a.mutate(func() {
			a.busy = false
			a.reachable = st != nil
			switch {
			case st != nil:
				a.st = *st
			case err != nil:
				a.st = status{State: stateError, LastError: err.Error()}
			}
		})
	}()
}

// toggle is what a left click on the icon does.
func (a *app) toggle() {
	a.mu.Lock()
	state, busy, reachable := a.st.State, a.busy, a.reachable
	a.mu.Unlock()

	switch {
	case busy || state == stateReconnecting:
		// Mid-operation; a second click should not queue another. A reconnect
		// the daemon started counts as mid-operation even though this process
		// is idle: clicking Connect would race the recovery for the same
		// tunnel, and clicking Disconnect would fight it.
	case !reachable:
		// There is nobody to ask. Starting a connect here would spend two
		// minutes failing against a socket that is not answering, and would
		// hide the "daemon not reachable" line the user needs to see.
	case state == stateConnected:
		a.doDisconnect()
	default:
		a.doConnect("")
	}
}

// copyExitIP puts the current exit address on the clipboard, if any tool on
// this machine can do it. Returns whether it worked.
//
// Shelling out is deliberate: speaking the Wayland or X clipboard protocols
// directly would mean a second dependency and a per-compositor implementation,
// for one menu item.
func (a *app) copyExitIP() bool {
	a.mu.Lock()
	ip := a.st.ExitIP
	a.mu.Unlock()
	if ip == "" {
		return false
	}

	for _, tool := range [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
	} {
		bin, err := exec.LookPath(tool[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(bin, tool[1:]...)
		cmd.Stdin = strings.NewReader(ip)
		if err := cmd.Run(); err == nil {
			return true
		}
	}
	return false
}

// clipboardTool reports whether any supported clipboard tool is present, so the
// menu item can be shown disabled rather than silently doing nothing.
func clipboardTool() bool {
	for _, name := range []string{"wl-copy", "xclip", "xsel"} {
		if _, err := exec.LookPath(name); err == nil {
			return true
		}
	}
	return false
}

func (a *app) quitNow() {
	a.quitOnce.Do(func() { close(a.quit) })
}
