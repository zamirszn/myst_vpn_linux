package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeClient stands in for the daemon. block, when set, holds a connect open so
// a test can observe the in-flight state.
type fakeClient struct {
	mu sync.Mutex

	statusResp *status
	statusErr  error

	connectResp *status
	connectErr  error

	disconnectResp *status
	disconnectErr  error

	statusCalls  int
	connectCalls int
	lastCountry  string

	block chan struct{}
}

func (f *fakeClient) status(context.Context) (*status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	return f.statusResp, f.statusErr
}

func (f *fakeClient) connect(_ context.Context, country, _ string) (*status, error) {
	f.mu.Lock()
	f.connectCalls++
	f.lastCountry = country
	block := f.block
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	return f.connectResp, f.connectErr
}

func (f *fakeClient) disconnect(context.Context) (*status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disconnectResp, f.disconnectErr
}

func (f *fakeClient) counts() (statusCalls, connectCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusCalls, f.connectCalls
}

// recorder captures emitted signals so tests can assert on them.
type recorder struct {
	mu   sync.Mutex
	sigs []string
}

func (r *recorder) emit(_ dbus.ObjectPath, name string, _ ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sigs = append(r.sigs, name)
	return nil
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sigs
	r.sigs = nil
	return out
}

func newTestApp(c daemonClient, country string) (*app, *recorder) {
	rec := &recorder{}
	a := newApp(nil, c, country)
	a.emitTo = rec.emit
	return a, rec
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (a *app) state() (status, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st, a.reachable, a.busy
}

// --- icon rendering -------------------------------------------------------

func alphaAt(p pixmap, x, y int) byte {
	return p.Data[(y*int(p.Width)+x)*4]
}

func TestRenderProducesTheRequestedGeometry(t *testing.T) {
	for _, size := range iconSizes {
		p := render(iconStyle{fill: colConnected, solid: true}, size)
		if int(p.Width) != size || int(p.Height) != size {
			t.Errorf("size %d: geometry = %dx%d", size, p.Width, p.Height)
		}
		if len(p.Data) != size*size*4 {
			t.Errorf("size %d: %d bytes of pixel data, want %d", size, len(p.Data), size*size*4)
		}
	}
}

func TestSolidIconCoversItsCentreAndNotItsCorners(t *testing.T) {
	// The shield tapers, so the centre of the canvas is inside it and the four
	// corners are not. If this inverts, the geometry constants are wrong.
	p := render(iconStyle{fill: colConnected, solid: true}, 32)

	if a := alphaAt(p, 16, 16); a != 255 {
		t.Errorf("centre alpha = %d, want 255", a)
	}
	for _, c := range [][2]int{{0, 0}, {31, 0}, {0, 31}, {31, 31}} {
		if a := alphaAt(p, c[0], c[1]); a != 0 {
			t.Errorf("corner (%d,%d) alpha = %d, want 0", c[0], c[1], a)
		}
	}
}

func TestHollowIconIsEmptyInTheMiddle(t *testing.T) {
	p := render(iconStyle{fill: colDisconnect, solid: false}, 32)

	if a := alphaAt(p, 16, 16); a != 0 {
		t.Errorf("hollow centre alpha = %d, want 0 — the outline is filled in", a)
	}
	// The outline itself has to still be there, or the icon is invisible.
	var opaque int
	for _, row := range [][]int{{10}, {14}} {
		for x := 0; x < 32; x++ {
			if alphaAt(p, x, row[0]) > 0 {
				opaque++
			}
		}
	}
	if opaque == 0 {
		t.Error("the hollow shield drew nothing at all")
	}
}

func TestIconPixelsAreBigEndianARGB(t *testing.T) {
	// A fully covered pixel must be opaque, and the colour channels must land in
	// R, G, B order. Getting the order wrong produces a plausible-looking but
	// wrong-coloured icon, which is exactly the kind of thing that survives a
	// glance.
	p := render(iconStyle{fill: rgb{0x11, 0x22, 0x33}, solid: true}, 32)
	i := (16*32 + 16) * 4

	if p.Data[i] != 255 {
		t.Errorf("alpha = %d, want 255", p.Data[i])
	}
	if p.Data[i+1] != 0x11 || p.Data[i+2] != 0x22 || p.Data[i+3] != 0x33 {
		t.Errorf("colour = %02x%02x%02x, want 112233", p.Data[i+1], p.Data[i+2], p.Data[i+3])
	}
}

func TestEachStateDrawsADifferentIcon(t *testing.T) {
	// The icon is the primary signal; if two states render identically the
	// applet silently stops communicating one of them.
	seen := map[string][]byte{}
	for _, tc := range []struct {
		name      string
		state     string
		reachable bool
	}{
		{"connected", stateConnected, true},
		{"connecting", stateConnecting, true},
		{"disconnected", stateDisconnected, true},
		{"error", stateError, true},
		{"unreachable", stateDisconnected, false},
	} {
		p := render(styleFor(tc.state, tc.reachable), 32)
		for other, data := range seen {
			if bytes.Equal(data, p.Data) {
				t.Errorf("%s and %s render identically", tc.name, other)
			}
		}
		seen[tc.name] = p.Data
	}
}

func TestStyleForMarksTheStates(t *testing.T) {
	if s := styleFor(stateConnected, true); !s.solid || !s.keyhole {
		t.Error("connected is not drawn as a solid keyhole shield")
	}
	if s := styleFor(stateDisconnected, true); s.solid {
		t.Error("disconnected is drawn solid, so it reads as connected")
	}
	if s := styleFor(stateError, true); !s.slash {
		t.Error("error is not marked with a slash")
	}
	// An unreachable daemon must not render as a healthy shield: it carries the
	// slash, and it is not the green keyhole of a live tunnel.
	if s := styleFor(stateConnected, false); !s.slash || s.keyhole {
		t.Error("an unreachable daemon is drawn as a working tunnel")
	}
}

// --- menu -----------------------------------------------------------------

func itemProps(t *testing.T, root layoutItem, id int32) map[string]dbus.Variant {
	t.Helper()
	it, ok := findItem(root, id)
	if !ok {
		t.Fatalf("the menu has no item with id %d", id)
	}
	return it.Properties
}

func boolProp(t *testing.T, props map[string]dbus.Variant, name string) bool {
	t.Helper()
	v, ok := props[name]
	if !ok {
		t.Fatalf("item has no property %q", name)
	}
	b, ok := v.Value().(bool)
	if !ok {
		t.Fatalf("property %q holds %T, want bool", name, v.Value())
	}
	return b
}

func stringProp(t *testing.T, props map[string]dbus.Variant, name string) string {
	t.Helper()
	v, ok := props[name]
	if !ok {
		t.Fatalf("item has no property %q", name)
	}
	s, ok := v.Value().(string)
	if !ok {
		t.Fatalf("property %q holds %T, want string", name, v.Value())
	}
	return s
}

func TestMenuOffersConnectWhenDownAndDisconnectWhenUp(t *testing.T) {
	c := &fakeClient{statusResp: &status{State: stateDisconnected}}
	a, _ := newTestApp(c, "gb")
	m := &menuMethods{a}

	props := itemProps(t, m.buildLocked(), idConnect)
	if !boolProp(t, props, "enabled") {
		t.Error("Connect is disabled while disconnected")
	}
	props = itemProps(t, m.buildLocked(), idDisconnect)
	if boolProp(t, props, "enabled") {
		t.Error("Disconnect is enabled while disconnected")
	}

	a.mutate(func() { a.st = status{State: stateConnected} })

	props = itemProps(t, m.buildLocked(), idConnect)
	if boolProp(t, props, "enabled") {
		t.Error("Connect is still enabled while connected")
	}
	props = itemProps(t, m.buildLocked(), idDisconnect)
	if !boolProp(t, props, "enabled") {
		t.Error("Disconnect is disabled while connected")
	}
}

func TestMenuChecksTheSelectedRegion(t *testing.T) {
	c := &fakeClient{statusResp: &status{State: stateDisconnected}}
	a, _ := newTestApp(c, "de")
	m := &menuMethods{a}

	root := m.buildLocked()
	for i, r := range regions {
		props := itemProps(t, root, idRegionBase+int32(i))
		state, ok := props["toggle-state"].Value().(int32)
		if !ok {
			t.Fatalf("region %s has no integer toggle-state", r.code)
		}
		want := int32(0)
		if r.code == "de" {
			want = 1
		}
		if state != want {
			t.Errorf("region %s toggle-state = %d, want %d", r.code, state, want)
		}
	}
}

func TestMenuDisablesCopyWithoutAnExitIP(t *testing.T) {
	c := &fakeClient{}
	a, _ := newTestApp(c, "gb")
	m := &menuMethods{a}

	// Connected but no exit IP reported: nothing to copy.
	a.mutate(func() { a.st = status{State: stateConnected} })
	props := itemProps(t, m.buildLocked(), idCopyIP)
	if boolProp(t, props, "enabled") {
		t.Error("Copy exit IP is enabled with no address to copy")
	}
}

func TestMenuSeparatorsAreMarked(t *testing.T) {
	c := &fakeClient{}
	a, _ := newTestApp(c, "gb")
	m := &menuMethods{a}

	props := itemProps(t, m.buildLocked(), idSepTop)
	if got := stringProp(t, props, "type"); got != "separator" {
		t.Errorf("type = %q, want separator", got)
	}
}

func TestQuitIsAlwaysAvailable(t *testing.T) {
	// The applet must be quittable even when the daemon is unreachable,
	// otherwise the only way out is killing the process.
	c := &fakeClient{statusErr: errors.New("no daemon")}
	a, _ := newTestApp(c, "gb")
	a.mutate(func() { a.reachable = false })

	m := &menuMethods{a}
	if !boolProp(t, itemProps(t, m.buildLocked(), idQuit), "enabled") {
		t.Error("Quit is disabled while the daemon is unreachable")
	}
}

// --- signals --------------------------------------------------------------

func TestASuccessfulConnectEmitsTheFullSet(t *testing.T) {
	c := &fakeClient{connectResp: &status{State: stateConnected, Country: "gb", ExitIP: "203.0.113.7"}}
	a, rec := newTestApp(c, "gb")
	rec.take()

	a.doConnect("")
	waitFor(t, "the connect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})

	got := strings.Join(rec.take(), ",")
	for _, want := range []string{"NewIcon", "NewStatus", "NewToolTip", "LayoutUpdated"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %s emitted on connect; got %q", want, got)
		}
	}
}

func TestARepeatPollEmitsNothing(t *testing.T) {
	// The poll runs every few seconds forever. If an unchanged poll emitted
	// signals, the panel would redraw continuously for no reason.
	same := &status{State: stateConnected, Country: "gb", ExitIP: "203.0.113.7"}
	c := &fakeClient{statusResp: same}
	a, rec := newTestApp(c, "gb")

	a.refresh(context.Background())
	if got := rec.take(); len(got) == 0 {
		t.Fatal("the first poll emitted nothing, so the icon never updates")
	}

	a.refresh(context.Background())
	if got := rec.take(); len(got) != 0 {
		t.Errorf("an unchanged poll emitted %v, want nothing", got)
	}
}

func TestTheIconFollowsADaemonThatVanishes(t *testing.T) {
	c := &fakeClient{statusResp: &status{State: stateConnected, ExitIP: "203.0.113.7"}}
	a, rec := newTestApp(c, "gb")
	a.refresh(context.Background())
	rec.take()

	// The socket goes away: no answer at all, as opposed to an error status.
	c.mu.Lock()
	c.statusResp, c.statusErr = nil, errors.New("dial unix: no such file")
	c.mu.Unlock()

	a.refresh(context.Background())

	_, reachable, _ := a.state()
	if reachable {
		t.Error("the applet still believes the daemon is reachable")
	}

	// st is only meaningful while reachable, so what matters is that nothing
	// presented to the user still claims a live tunnel.
	a.mu.Lock()
	iconFor, tip, sniStatus := a.iconFor, a.tooltip, a.sniStatus
	a.mu.Unlock()

	if iconFor != "unreachable" {
		t.Errorf("icon state = %q, want unreachable — a stale tunnel is still on screen", iconFor)
	}
	if !strings.Contains(tip, "not reachable") {
		t.Errorf("tooltip = %q, does not say the daemon is unreachable", tip)
	}
	if sniStatus != "NeedsAttention" {
		t.Errorf("SNI status = %q, want NeedsAttention", sniStatus)
	}
	if got := strings.Join(rec.take(), ","); !strings.Contains(got, "NewIcon") {
		t.Errorf("losing the daemon did not update the icon; got %q", got)
	}
}

func TestAFailedConnectDoesNotClaimToBeConnected(t *testing.T) {
	c := &fakeClient{connectErr: errors.New("account has no device slots left")}
	a, _ := newTestApp(c, "gb")

	a.doConnect("")
	waitFor(t, "the connect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})

	st, _, _ := a.state()
	if st.State == stateConnected {
		t.Error("a failed connect left the applet reporting a live tunnel")
	}
	if !strings.Contains(st.LastError, "no device slots") {
		t.Errorf("LastError = %q, does not explain the failure", st.LastError)
	}
}

func TestAConnectErrorFromTheDaemonKeepsItsStatus(t *testing.T) {
	// The daemon answers an error with a status attached. Reporting
	// "unreachable" there would be wrong: it answered.
	c := &fakeClient{
		connectResp: &status{State: stateError, LastError: "nft: permission denied"},
		connectErr:  errors.New("nft: permission denied"),
	}
	a, _ := newTestApp(c, "gb")

	a.doConnect("")
	waitFor(t, "the connect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})

	st, reachable, _ := a.state()
	if !reachable {
		t.Error("the applet treats a daemon error reply as the daemon being gone")
	}
	if st.State != stateError {
		t.Errorf("state = %q, want %q", st.State, stateError)
	}
}

func TestASecondClickIsIgnoredWhileBusy(t *testing.T) {
	// Connect takes up to a minute. Without the guard, an impatient user
	// queues a second one behind the first.
	block := make(chan struct{})
	c := &fakeClient{connectResp: &status{State: stateConnected}, block: block}
	a, _ := newTestApp(c, "gb")

	a.doConnect("")
	// busy is set synchronously but the call itself lands on a goroutine, so
	// wait for it to actually reach the client before counting.
	waitFor(t, "the first connect to reach the daemon", func() bool {
		_, calls := c.counts()
		return calls == 1
	})

	a.doConnect("de")
	a.toggle()

	if _, calls := c.counts(); calls != 1 {
		t.Errorf("connect called %d times, want 1", calls)
	}

	close(block)
	waitFor(t, "the connect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})
}

func TestPollingIsSkippedWhileBusy(t *testing.T) {
	// The daemon holds one mutex across a connect, so a status call during one
	// would block and then time out — indistinguishable from a dead daemon,
	// which would flash the icon to grey mid-connect.
	block := make(chan struct{})
	c := &fakeClient{connectResp: &status{State: stateConnected}, block: block}
	a, _ := newTestApp(c, "gb")

	a.doConnect("")
	waitFor(t, "the connect to start", func() bool {
		_, _, busy := a.state()
		return busy
	})

	a.refresh(context.Background())

	if calls, _ := c.counts(); calls != 0 {
		t.Errorf("polled the daemon %d times mid-connect, want 0", calls)
	}

	close(block)
	waitFor(t, "the connect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})
}

func TestToggleConnectsWhenDownAndDisconnectsWhenUp(t *testing.T) {
	c := &fakeClient{
		statusResp:     &status{State: stateDisconnected},
		connectResp:    &status{State: stateConnected},
		disconnectResp: &status{State: stateDisconnected},
	}
	a, _ := newTestApp(c, "gb")

	// A click only does something once the daemon has answered at least once,
	// which in the real applet is the poll that runs before the event loop.
	a.refresh(context.Background())

	a.toggle()
	waitFor(t, "the connect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})
	if _, calls := c.counts(); calls != 1 {
		t.Fatalf("toggle while down made %d connect calls, want 1", calls)
	}

	a.toggle()
	waitFor(t, "the disconnect to settle", func() bool {
		_, _, busy := a.state()
		return !busy
	})

	st, _, _ := a.state()
	if st.State != stateDisconnected {
		t.Errorf("state after toggling off = %q, want %q", st.State, stateDisconnected)
	}
}

func TestQuitSignalsOnce(t *testing.T) {
	c := &fakeClient{}
	a, _ := newTestApp(c, "gb")

	a.quitNow()
	a.quitNow() // must not panic on a double close

	select {
	case <-a.quit:
	default:
		t.Error("quit did not close the channel")
	}
}

// --- helpers --------------------------------------------------------------

func TestRegionNameFallsBackToTheCode(t *testing.T) {
	if got := regionName("gb"); got != "United Kingdom" {
		t.Errorf("regionName(gb) = %q", got)
	}
	if got := regionName("GB"); got != "United Kingdom" {
		t.Errorf("regionName(GB) = %q — matching should ignore case", got)
	}
	// An unlisted code is not an error: it may be a perfectly valid country
	// that this curated list does not carry.
	if got := regionName("zz"); got != "ZZ" {
		t.Errorf("regionName(zz) = %q, want ZZ", got)
	}
	if got := regionName(""); got == "" {
		t.Error("regionName(\"\") is empty, so the tooltip reads badly")
	}
}

func TestTitleCase(t *testing.T) {
	for in, want := range map[string]string{
		"gloucester":        "Gloucester",
		"new york":          "New York",
		"frankfurt am main": "Frankfurt Am Main",
		"":                  "",
	} {
		if got := titleCase(in); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", in, got, want)
		}
	}
}
