package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/zamirszn/mysterium-linux-client/desktop/internal/daemonclient"
	"github.com/zamirszn/mysterium-linux-client/desktop/internal/regions"
)

// pollInterval is how often the daemon is asked for its state. There is no push
// channel, so this is the only way the window learns about a change made
// elsewhere — from the CLI, from the tray, or by the health monitor repairing a
// tunnel on its own.
const pollInterval = 2 * time.Second

// opTimeout bounds one connect or disconnect. The daemon allows itself two
// minutes of recovery, so a client that gave up sooner would abandon operations
// that were about to succeed.
const opTimeout = 3 * time.Minute

// fastestLabel is the region picker's first entry. It sends no country at all,
// which asks the API to choose the best node.
const fastestLabel = "Fastest (recommended)"

// fallbackNote is shown while the picker is on the built-in list rather than
// the account's own. It names the likely cause, because the likely cause is a
// daemon older than this window, and that is not something the window can fix
// by trying harder.
const fallbackNote = `<span foreground="#888" size="small">Common regions only — ` +
	`the daemon could not list them all. If mystd is up to date, either the ` +
	`list is still loading or the daemon is not answering.</span>`

type window struct {
	app    *gtk.Application
	client daemonclient.Client

	win     *gtk.ApplicationWindow
	heading *gtk.Label
	detail  *gtk.Label
	action  *gtk.Button
	region  *gtk.DropDown
	note    *gtk.Label
	exitIP  *gtk.Label
	copy    *gtk.Button
	iface   *gtk.Label
	ipv6    *gtk.Label
	ks      *gtk.Label

	// busy is true while a connect or disconnect is in flight. The daemon
	// publishes its own state for the button's label, but only this knows when
	// to hand the button back after a failure that never reached the daemon.
	busy atomic.Bool

	// polling keeps one status request in flight at a time, so a daemon that
	// has stopped answering cannot accumulate a goroutine per tick.
	polling atomic.Bool

	// generation counts completed operations. A status request records it
	// before it dials, and is discarded if it changed while the request was in
	// flight — otherwise a poll sent during a connect could land afterwards and
	// put "connecting" back on screen over a tunnel that is already up.
	generation atomic.Uint64

	// shown is the status currently rendered. Only the main loop touches it.
	shown daemonclient.Status

	// choices is the country list the picker currently shows, excluding the
	// leading "Fastest" entry. It starts as the built-in fallback and is
	// replaced by the real list once the daemon answers.
	choices []regions.Region

	// haveCountries records whether choices came from the daemon. Until it does,
	// every poll also asks for the list, so a window opened before mystd was
	// running fills in as soon as it can.
	haveCountries atomic.Bool

	// loadingCountries keeps one countries request in flight at a time. Without
	// it a daemon that accepts connections but never answers would accumulate a
	// goroutine per poll.
	loadingCountries atomic.Bool
}

func newWindow(app *gtk.Application, client daemonclient.Client) *window {
	w := &window{app: app, client: client}

	w.win = gtk.NewApplicationWindow(app)
	w.win.SetTitle("Mysterium VPN")
	w.win.SetDefaultSize(400, 300)

	w.heading = gtk.NewLabel("…")
	w.heading.SetMarkup("<b>Checking…</b>")
	w.heading.SetHAlign(gtk.AlignStart)

	w.detail = gtk.NewLabel("")
	w.detail.SetWrap(true)
	w.detail.SetHAlign(gtk.AlignStart)
	w.detail.SetSelectable(true)

	w.action = gtk.NewButtonWithLabel("Connect")
	w.action.ConnectClicked(func() { w.toggle() })

	// Entry 0 is "Fastest" and the regions follow it. Without that leading
	// entry the picker opens on whichever region happens to be first in the
	// list, so a first-run click on Connect silently asks for that one country
	// — and fails outright whenever that country has no node free. Asking for
	// nothing is the safe default, and it is what the CLI already does.
	//
	// The list starts as the built-in fallback so the window opens immediately
	// rather than waiting on the daemon; the real list, which is the account's
	// own and about five times longer, replaces it as soon as it arrives.
	w.choices = regions.All
	w.region = gtk.NewDropDownFromStrings(w.pickerLabels())

	// Says so when the picker is on the built-in fallback rather than the
	// account's real list. Without it the two are indistinguishable, and a
	// daemon too old to answer "countries" — which is exactly what an
	// un-upgraded mystd is — looks like a client that only knows eighteen
	// countries.
	w.note = gtk.NewLabel("")
	w.note.SetWrap(true)
	w.note.SetHAlign(gtk.AlignStart)
	w.note.SetMarkup(fallbackNote)

	w.copy = gtk.NewButtonWithLabel("Copy")
	w.copy.ConnectClicked(func() { w.copyExitIP() })

	body := gtk.NewBox(gtk.OrientationVertical, 18)
	body.SetMarginTop(18)
	body.SetMarginBottom(18)
	body.SetMarginStart(18)
	body.SetMarginEnd(18)

	body.Append(w.heading)
	body.Append(w.detail)
	body.Append(w.action)
	body.Append(w.region)
	body.Append(w.note)
	body.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	body.Append(w.details())

	w.win.SetChild(body)

	// Ask immediately so the window opens on the real state rather than a
	// placeholder, then keep asking.
	w.poll()
	glib.TimeoutAdd(uint(pollInterval.Milliseconds()), func() bool {
		w.poll()
		return true // keep the timer running
	})

	return w
}

// details builds the read-only rows: the exit IP with its copy action, and the
// three indicators worth being able to check at a glance.
func (w *window) details() *gtk.Widget {
	grid := gtk.NewGrid()
	grid.SetRowSpacing(8)
	grid.SetColumnSpacing(16)

	w.exitIP = gtk.NewLabel("—")
	w.iface = gtk.NewLabel("—")
	w.ipv6 = gtk.NewLabel("—")
	w.ks = gtk.NewLabel("—")

	rows := []struct {
		key   string
		value *gtk.Label
	}{
		{"Exit IP", w.exitIP},
		{"Interface", w.iface},
		{"IPv6", w.ipv6},
		{"Kill switch", w.ks},
	}
	for i, row := range rows {
		key := gtk.NewLabel(row.key)
		key.SetHAlign(gtk.AlignStart)
		// Muted, so the values carry the eye.
		key.SetMarkup(fmt.Sprintf("<span foreground=\"#888\">%s</span>", row.key))

		row.value.SetHAlign(gtk.AlignStart)
		row.value.SetSelectable(true)

		grid.Attach(key, 0, i, 1, 1)
		grid.Attach(row.value, 1, i, 1, 1)
	}

	// The copy button sits beside the IP it copies rather than in a row of its
	// own, so there is no doubt about what it acts on.
	w.copy.SetHAlign(gtk.AlignEnd)
	grid.Attach(w.copy, 2, 0, 1, 1)

	return &grid.Widget
}

func (w *window) present() { w.win.Present() }

// onMain runs f on the GTK main loop. Every widget touch has to go through
// this: the socket calls run on their own goroutines, and GTK is not
// thread-safe.
func (w *window) onMain(f func()) { glib.IdleAdd(f) }

// poll asks the daemon for its state without blocking the main loop.
func (w *window) poll() {
	// The country list is asked for on the same tick until it arrives, so a
	// window opened before the daemon started is not stuck on the fallback list
	// for the rest of the session.
	w.loadCountries()

	if !w.polling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer w.polling.Store(false)

		gen := w.generation.Load()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		st, err := w.client.Status(ctx)
		w.onMain(func() {
			if err != nil {
				w.showUnreachable(err)
				return
			}
			if w.generation.Load() != gen {
				// An operation finished while this was in flight, so this
				// answer is older than what is already on screen.
				return
			}
			w.apply(*st)
		})
	}()
}

// toggle is the one button: it connects when there is no tunnel and tears one
// down when there is.
func (w *window) toggle() {
	if w.busy.Load() {
		return
	}
	// The region that is on screen decides, not the picker: while connected the
	// picker is disabled and shows whatever it was left on.
	if w.onScreen().State == daemonclient.StateConnected {
		w.run(func(ctx context.Context) (*daemonclient.Status, error) {
			return w.client.Disconnect(ctx)
		})
		return
	}
	w.run(func(ctx context.Context) (*daemonclient.Status, error) {
		return w.client.Connect(ctx, w.selectedRegion(), "")
	})
}

// selectedRegion is the country code currently chosen in the picker, or "" to
// let the API choose the fastest.
func (w *window) selectedRegion() string { return regionCodeAt(w.choices, int(w.region.Selected())) }

// fillRegions replaces the picker's contents with a country list, keeping the
// "Fastest" entry at row 0.
//
// The list is rebuilt rather than patched because the model is replaced
// wholesale; the selection is resolved back from the country code it was on, so
// refreshing underneath the user does not silently move their choice.
func (w *window) fillRegions(list []regions.Region) {
	if len(list) == 0 {
		// Nothing to offer: keep whatever is already there rather than emptying
		// the picker, which would look like the window had broken.
		return
	}
	was := w.selectedRegion()
	w.choices = list

	w.region.SetModel(gtk.NewStringList(w.pickerLabels()))
	w.region.SetSelected(regionRow(list, was))

	// The real list is on screen, so the disclaimer about not having it is no
	// longer true.
	w.note.SetVisible(false)
}

// pickerLabels is the dropdown's row labels: "Fastest", then the country names.
func (w *window) pickerLabels() []string {
	names := make([]string, 0, len(w.choices)+1)
	names = append(names, fastestLabel)
	for _, r := range w.choices {
		names = append(names, r.Name)
	}
	return names
}

// regionCodeAt maps a picker row to a country code, and regionRow does the
// reverse. They are the only place the "Fastest" offset lives, because an
// off-by-one here would silently connect the user to the wrong country.
//
// They take the list rather than reading a global so they stay pure, and
// therefore testable without a display.
func regionCodeAt(list []regions.Region, row int) string {
	// Row 0 is "Fastest" and a fresh dropdown reports -1, so both mean the same
	// thing: ask for no country in particular.
	if row <= 0 || row > len(list) {
		return ""
	}
	return list[row-1].Code
}

func regionRow(list []regions.Region, code string) uint {
	if code == "" {
		return 0
	}
	for i, r := range list {
		// Case-insensitive for the same reason regions.Name is: the status
		// carries the API's canonical code, and a mismatch here would silently
		// drop the picker back to "Fastest" while connected to a country.
		if strings.EqualFold(r.Code, code) {
			return uint(i + 1) // +1 for the leading "Fastest" entry
		}
	}
	return 0
}

// run performs one daemon operation off the main loop. A connect provisions a
// tunnel upstream and can take minutes; doing that on the main loop would
// freeze the window for exactly as long as the user is waiting to see what
// happened.
func (w *window) run(op func(context.Context) (*daemonclient.Status, error)) {
	if !w.busy.CompareAndSwap(false, true) {
		return
	}
	w.action.SetSensitive(false)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()

		st, err := op(ctx)

		w.onMain(func() {
			// Whatever happened, this result is newer than any poll in flight.
			w.generation.Add(1)
			w.busy.Store(false)
			w.action.SetSensitive(true)

			switch {
			case err != nil && st != nil:
				// The daemon answered with a failure, so its status is the
				// better explanation than the transport error.
				w.apply(*st)
			case err != nil:
				w.showUnreachable(err)
			default:
				w.apply(*st)
			}
		})
	}()
}

// onScreen is the status currently rendered.
func (w *window) onScreen() daemonclient.Status { return w.shown }

// apply renders a status from the daemon. It is the only place the widgets are
// written from a status, so the window cannot disagree with itself.
func (w *window) apply(st daemonclient.Status) {
	w.shown = st

	switch st.State {
	case daemonclient.StateConnected:
		w.heading.SetMarkup("<b>Connected</b>")
		w.detail.SetText(w.regionLine(st))
	case daemonclient.StateConnecting:
		w.heading.SetMarkup("<b>Connecting…</b>")
		w.detail.SetText("Bringing the tunnel up. This can take a moment.")
	case daemonclient.StateReconnecting:
		w.heading.SetMarkup("<b>Reconnecting…</b>")
		w.detail.SetText(w.reconnectLine(st))
	default:
		w.heading.SetMarkup("<b>Not connected</b>")
		w.detail.SetText(w.idleLine(st))
	}

	// The button follows the state, except while an operation of ours is in
	// flight and the daemon has not caught up yet.
	//
	// Connecting and reconnecting get different labels on purpose: one is the
	// user's own request in progress, the other is the daemon repairing a tunnel
	// that died under them. "Working…" for both would hide the difference at the
	// exact moment the user is wondering what went wrong.
	busy := st.State == daemonclient.StateConnecting || st.State == daemonclient.StateReconnecting
	switch {
	case st.State == daemonclient.StateReconnecting:
		w.action.SetLabel("Reconnecting…")
		w.action.SetSensitive(false)
	case st.State == daemonclient.StateConnecting:
		w.action.SetLabel("Connecting…")
		w.action.SetSensitive(false)
	case st.State == daemonclient.StateConnected:
		w.action.SetLabel("Disconnect")
		w.action.SetSensitive(!w.busy.Load())
	default:
		w.action.SetLabel("Connect")
		w.action.SetSensitive(!w.busy.Load())
	}

	// A region is chosen before connecting and cannot be changed underneath a
	// live tunnel, so the picker is live only when there is nothing to disturb.
	w.region.SetSensitive(!busy && st.State != daemonclient.StateConnected)
	if st.State == daemonclient.StateConnected {
		w.selectRegion(st.Country)
	}

	w.exitIP.SetText(orDash(st.ExitIP))
	w.copy.SetSensitive(st.State == daemonclient.StateConnected && st.ExitIP != "")
	w.iface.SetText(orDash(st.Interface))

	if st.State != daemonclient.StateConnected {
		w.ipv6.SetText("—")
		w.ks.SetText("—")
		return
	}
	w.ipv6.SetText(yesNo(st.IPv6Routed, "routed", "NOT routed — IPv6 bypasses the tunnel"))
	w.ks.SetText(yesNo(st.KillSwitch, "armed", "not armed"))
}

// selectRegion moves the picker to a country code, so the window shows the
// region actually in use rather than the one that happened to be highlighted.
func (w *window) selectRegion(code string) { w.region.SetSelected(regionRow(w.choices, code)) }

// loadCountries pulls the account's country list from the daemon.
//
// It runs off the main loop like every other daemon call, and is harmless to
// repeat: it is only asked for until it succeeds. The built-in fallback stays
// on screen until then, so a window opened before the daemon is up still works.
func (w *window) loadCountries() {
	if w.haveCountries.Load() {
		return
	}
	if !w.loadingCountries.CompareAndSwap(false, true) {
		return // one attempt in flight is enough
	}
	go func() {
		defer w.loadingCountries.Store(false)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		codes, err := w.client.Countries(ctx)
		if err != nil || len(codes) == 0 {
			// Not worth showing the user: they already see the fallback list,
			// and the status poll reports a daemon that is not answering.
			return
		}
		list := regions.FromCodes(codes)
		w.onMain(func() {
			if w.haveCountries.Swap(true) {
				return // a slower earlier request beat us to it
			}
			w.fillRegions(list)
		})
	}()
}

// showUnreachable is for a daemon that did not answer at all, which is a
// different problem from a daemon that answered with a failure.
func (w *window) showUnreachable(err error) {
	w.action.SetSensitive(true)
	w.action.SetLabel("Connect")
	w.heading.SetMarkup("<b>Cannot reach the daemon</b>")
	w.detail.SetText(fmt.Sprintf("%v\n\nIs mystd running?  systemctl status mystd", err))
	w.exitIP.SetText("—")
	w.iface.SetText("—")
	w.ipv6.SetText("—")
	w.ks.SetText("—")
	w.copy.SetSensitive(false)
}

// copyExitIP puts the exit IP on the clipboard.
//
// Unlike the tray, which shells out to wl-copy or xclip, this uses GTK's own
// clipboard — the window is already a GTK process, so there is nothing to
// install and nothing to find on the PATH.
func (w *window) copyExitIP() {
	ip := w.onScreen().ExitIP
	if ip == "" {
		return
	}
	w.win.Clipboard().SetText(ip)

	w.copy.SetLabel("Copied")
	glib.TimeoutAdd(1500, func() bool {
		w.copy.SetLabel("Copy")
		return false // one shot, not a repeating timer
	})
}

func (w *window) regionLine(st daemonclient.Status) string {
	name := regions.Name(st.Country)
	if st.City != "" {
		name += " · " + regions.TitleCase(st.City)
	}
	if st.IPType != "" {
		name += " · " + st.IPType
	}
	return name
}

func (w *window) reconnectLine(st daemonclient.Status) string {
	if st.LastError != "" {
		return st.LastError
	}
	return "The tunnel stopped working. Rebuilding it now."
}

// idleLine explains a state that is not connected. An error is the one worth
// reading closely: when recovery gives up, the daemon puts the reason the
// machine is back on the open network into this text.
func (w *window) idleLine(st daemonclient.Status) string {
	if st.LastError != "" {
		return st.LastError
	}
	return "Your traffic is going out directly."
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}
