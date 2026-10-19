package main

import (
	"context"
	"log"
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
const fastestLabel = "Fastest location"

// fallbackNote is shown while the picker is on the built-in list rather than
// the account's own. It names the likely cause, because the likely cause is a
// daemon older than this window, and that is not something the window can fix
// by trying harder.
const fallbackNote = `<span foreground="#a79fbe" size="small">Common regions only — ` +
	`the full location list is still loading. If this does not change, restart the VPN service.</span>`

type window struct {
	app    *gtk.Application
	client daemonclient.Client
	socket string

	win     *gtk.ApplicationWindow
	banner  *gtk.Label // the green/amber/red strip at the top of the main screen
	heading *gtk.Label
	detail  *gtk.Label
	action  *gtk.Button
	ipType  *gtk.Notebook
	region  *gtk.DropDown
	city    *gtk.DropDown
	search  *gtk.SearchEntry
	results *gtk.ListBox
	note    *gtk.Label
	iface   *gtk.Label
	ipv6    *gtk.Label
	ks      *gtk.Label

	// The lavender card over the map: where the tunnel goes and the one
	// button that acts on it.
	statusCard   *gtk.Box
	statusVisual *statusVisual
	locFlag      *gtk.Label
	locName      *gtk.Label
	locMeta      *gtk.Label
	exitLine     *gtk.Label
	copy         *gtk.Button
	repair       *gtk.Button

	// The window has two screens, and the daemon's answer to "is anyone signed
	// in?" chooses between them. See login.go.
	screens    *gtk.Stack
	login      loginView
	accountBar *gtk.Box
	account    *gtk.Label
	signOut    *gtk.Button

	// loginBusy is true while a login request is out, so a second click cannot
	// start a second link.
	loginBusy atomic.Bool

	// signOutArmed is whether the Sign out button is waiting for its second
	// click, and armSeq retires the timer that would put it back. Only the main
	// loop touches them.
	signOutArmed bool
	armSeq       int

	// notice is a message that has to survive the next poll: the detail line is
	// repainted from the daemon's status every two seconds, which would wipe an
	// error that has nowhere else to live. Cleared when the person acts again.
	// Only the main loop touches it.
	notice string

	// epoch counts account changes. A lookup records it before it starts and is
	// discarded if it moved, so a country list fetched for the account that just
	// signed out cannot fill the picker of the one that signed in. Only the main
	// loop touches it.
	epoch int

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

	// places is the country/city data once the daemon has answered "locations".
	// Cities are a separate, larger request from the plain country list, so a
	// daemon too old to serve them costs the window nothing beyond having no
	// city picker: countries alone keep working exactly as before.
	//
	// The residential and data centre lists are genuinely different datasets
	// (106 countries against 49), so each is fetched and kept separately.
	// places is the one for the type currently selected; placesByType holds
	// every list fetched so far. Only the main loop touches these.
	places       []regions.Place
	placesByType map[string][]regions.Place

	// loadingPlaces keeps one locations request in flight per type, for the
	// same reason loadingCountries does. Only the main loop touches it.
	loadingPlaces map[string]bool

	// cityChoice is the picked city's slug, remembered across a country change
	// so switching country and back does not lose it. Only the main loop
	// touches it.
	cityChoice string

	// ipTypeIdx is the selected IP type tab. Only the main loop touches it.
	ipTypeIdx int

	// matches are the rows currently in the search results list, in row order.
	// Only the main loop touches it.
	matches []regions.CityMatch
}

// searchLimit caps the results list so it stays a short menu under the entry
// rather than growing the window.
const searchLimit = 6

func newWindow(app *gtk.Application, client daemonclient.Client, socket string) *window {
	w := &window{
		app:           app,
		client:        client,
		socket:        socket,
		placesByType:  map[string][]regions.Place{},
		loadingPlaces: map[string]bool{},
	}

	// The stylesheet is installed per display, once the display exists — which
	// it does by the time the activate handler runs.
	loadStyle()

	w.win = gtk.NewApplicationWindow(app)
	w.win.SetTitle("Mysterium VPN")
	// A tall column, like the official app's connection screen.
	w.win.SetDefaultSize(460, 820)

	// Hidden until there is a tunnel (or one is being built) to report. The
	// main screen reuses it for the red "disconnected" strip.
	w.banner = gtk.NewLabel("")
	w.banner.AddCSSClass("status-strip")
	w.banner.SetHExpand(true)
	w.banner.SetHAlign(gtk.AlignFill)
	w.banner.SetVisible(false)

	w.heading = gtk.NewLabel("")
	w.heading.SetMarkup("<b>Checking…</b>")
	w.heading.SetHAlign(gtk.AlignStart)
	w.heading.AddCSSClass("status-heading")

	w.detail = gtk.NewLabel("")
	w.detail.SetWrap(true)
	w.detail.SetHAlign(gtk.AlignStart)
	w.detail.SetSelectable(true)
	w.detail.AddCSSClass("status-detail")

	w.action = gtk.NewButtonWithLabel("Connect")
	w.action.AddCSSClass("connect-btn")
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

	// Which pool of IPs to exit through, as a segmented control. The pages are
	// empty: the pickers below are shared, and switching tab swaps their lists
	// for that pool's own.
	w.ipType = gtk.NewNotebook()
	w.ipType.SetShowBorder(false)
	for _, label := range ipTypeLabels() {
		w.ipType.AppendPage(gtk.NewBox(gtk.OrientationVertical, 0), gtk.NewLabel(label))
	}
	// Connected after the pages exist so adding them does not fire it. The
	// signal runs before the notebook's own current page changes, so the row
	// is taken from its argument and kept in ipTypeIdx rather than read back.
	w.ipType.ConnectSwitchPage(func(_ gtk.Widgetter, page uint) {
		w.ipTypeIdx = int(page)
		w.onIPTypeChanged()
	})

	w.region = gtk.NewDropDownFromStrings(w.pickerLabels())
	w.region.SetHExpand(true)
	w.region.NotifyProperty("selected", func() {
		w.refreshCityPicker()
		w.applyCitySensitivity()
		w.refreshLocationCard()
	})

	// "Any city" is the only entry until a country with cities is picked.
	w.city = gtk.NewDropDownFromStrings([]string{"Any city"})
	w.city.SetHExpand(true)
	w.city.SetSensitive(false)
	w.city.NotifyProperty("selected", func() {
		cities := regions.CitiesOf(w.places, w.selectedRegion())
		w.cityChoice = regions.CitySlugAt(cities, int(w.city.Selected()))
		w.refreshLocationCard()
	})

	// Search across every country's cities. Picking a hit sets both dropdowns,
	// so the connect path is unchanged: it still reads them.
	w.search = gtk.NewSearchEntry()
	w.search.SetObjectProperty("placeholder-text", "Search locations")
	w.search.SetSensitive(false) // until the daemon's city list arrives
	w.search.ConnectSearchChanged(func() { w.showSearchResults() })

	w.results = gtk.NewListBox()
	w.results.SetSelectionMode(gtk.SelectionNone)
	w.results.SetActivateOnSingleClick(true)
	w.results.AddCSSClass("results")
	w.results.SetVisible(false)
	w.results.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		i := row.Index()
		if i < 0 || i >= len(w.matches) {
			return
		}
		w.pickSearchResult(w.matches[i])
	})

	// Says so when the picker is on the built-in fallback rather than the
	// account's real list.
	w.note = gtk.NewLabel("")
	w.note.SetWrap(true)
	w.note.SetHAlign(gtk.AlignStart)
	w.note.SetMarkup(fallbackNote)

	// The animated panel and the connection card that floats over its lower
	// edge. The overlay is what makes the card read as part of the map, the way
	// the official app's location sheet does.
	w.statusVisual = newStatusVisual()
	w.statusCard = w.buildStatusCard()

	panel := gtk.NewOverlay()
	panel.SetChild(w.statusVisual.area)
	panel.AddOverlay(w.statusCard)
	panel.SetVExpand(true)
	w.statusCard.SetHAlign(gtk.AlignFill)
	w.statusCard.SetVAlign(gtk.AlignEnd)
	w.statusCard.SetMarginStart(12)
	w.statusCard.SetMarginEnd(12)
	w.statusCard.SetMarginBottom(12)

	// Everything that chooses where to connect goes under one title.
	locationTitle := gtk.NewLabel("SELECT LOCATION")
	locationTitle.AddCSSClass("section-title")
	locationTitle.SetHAlign(gtk.AlignStart)

	detailsTitle := gtk.NewLabel("CONNECTION DETAILS")
	detailsTitle.AddCSSClass("section-title")
	detailsTitle.SetHAlign(gtk.AlignStart)

	detailsCard := gtk.NewBox(gtk.OrientationVertical, 0)
	detailsCard.AddCSSClass("card")
	detailsCard.Append(w.details())

	body := gtk.NewBox(gtk.OrientationVertical, 12)
	body.SetMarginTop(14)
	body.SetMarginBottom(20)
	body.SetMarginStart(18)
	body.SetMarginEnd(18)

	body.Append(w.search)
	body.Append(w.results)
	body.Append(panel)
	body.Append(locationTitle)
	body.Append(w.ipType)
	body.Append(w.region)
	body.Append(w.city)
	body.Append(w.note)
	body.Append(detailsTitle)
	body.Append(detailsCard)

	// Scrolls rather than growing the window, so a short screen can still
	// reach the details card.
	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetVExpand(true)
	scroller.SetChild(body)

	mainScreen := gtk.NewBox(gtk.OrientationVertical, 0)
	mainScreen.Append(w.banner)
	mainScreen.Append(scroller)

	// Which of the two is showing is the daemon's call, made in apply. It opens
	// on the login screen while the first status request is pending. A current
	// daemon switches to the correct screen as soon as its status arrives.
	w.screens = gtk.NewStack()
	w.screens.SetVExpand(true)
	w.screens.AddNamed(mainScreen, "main")
	w.screens.AddNamed(w.buildLogin(), "login")
	w.screens.SetVisibleChildName("login")

	root := gtk.NewBox(gtk.OrientationVertical, 0)
	root.Append(w.buildHeader())
	root.Append(w.screens)

	w.win.SetChild(root)

	// Ask immediately so the window opens on the real state rather than a
	// placeholder, then keep asking.
	w.poll()
	w.loadPlaces()
	glib.TimeoutAdd(uint(pollInterval.Milliseconds()), func() bool {
		w.poll()
		w.loadPlaces()
		return true // keep the timer running
	})
	// The map's pulse runs on its own faster clock; tick is a no-op unless a
	// tunnel is actually being built.
	glib.TimeoutAdd(60, func() bool {
		w.statusVisual.tick()
		return true
	})

	return w
}

// buildHeader is the top bar: the Mysterium mark and wordmark on the left, the
// signed-in account and sign-out action on the right.
func (w *window) buildHeader() *gtk.Box {
	header := gtk.NewBox(gtk.OrientationHorizontal, 11)
	header.AddCSSClass("header")

	if img := logoImage(26); img != nil {
		img.SetVAlign(gtk.AlignCenter)
		header.Append(img)
	}

	strong := gtk.NewLabel("MYSTERIUM")
	strong.AddCSSClass("brand-strong")
	light := gtk.NewLabel("VPN")
	light.AddCSSClass("brand-light")

	word := gtk.NewBox(gtk.OrientationHorizontal, 2)
	word.Append(strong)
	word.Append(light)

	sub := gtk.NewLabel("PRIVATE · FAST · GLOBAL")
	sub.AddCSSClass("brand-sub")
	sub.SetHAlign(gtk.AlignStart)

	brand := gtk.NewBox(gtk.OrientationVertical, 0)
	brand.Append(word)
	brand.Append(sub)
	brand.SetVAlign(gtk.AlignCenter)
	brand.SetHExpand(true)

	header.Append(brand)
	header.Append(w.buildAccountBar())
	w.accountBar.SetVisible(false)
	return header
}

// buildStatusCard is the lavender sheet: state, location, exit address and the
// one button that acts on the tunnel.
func (w *window) buildStatusCard() *gtk.Box {
	card := gtk.NewBox(gtk.OrientationVertical, 8)
	card.AddCSSClass("status-card")
	card.SetHExpand(true)

	card.Append(w.heading)
	card.Append(w.detail)

	w.locFlag = gtk.NewLabel("\U0001F310")
	w.locFlag.AddCSSClass("flag")
	w.locFlag.SetVAlign(gtk.AlignCenter)

	w.locName = gtk.NewLabel(fastestLabel)
	w.locName.AddCSSClass("location-name")
	w.locName.SetHAlign(gtk.AlignStart)

	w.locMeta = gtk.NewLabel("Automatic")
	w.locMeta.AddCSSClass("location-meta")
	w.locMeta.SetHAlign(gtk.AlignStart)

	info := gtk.NewBox(gtk.OrientationVertical, 0)
	info.SetHExpand(true)
	info.Append(w.locName)
	info.Append(w.locMeta)

	location := gtk.NewBox(gtk.OrientationHorizontal, 10)
	location.SetMarginTop(2)
	location.Append(w.locFlag)
	location.Append(info)

	card.Append(location)

	w.exitLine = gtk.NewLabel("Not connected")
	w.exitLine.AddCSSClass("exit-ip")
	w.exitLine.SetHAlign(gtk.AlignStart)
	w.exitLine.SetHExpand(true)

	w.copy = gtk.NewButtonWithLabel("Copy IP")
	w.copy.AddCSSClass("chip")
	w.copy.ConnectClicked(func() { w.copyExitIP() })

	exitRow := gtk.NewBox(gtk.OrientationHorizontal, 8)
	exitRow.Append(w.exitLine)
	exitRow.Append(w.copy)
	card.Append(exitRow)

	// Shown only when the daemon could not be reached. Without it the window is
	// a dead end: the person is told the service is not running and has no way
	// to start it without opening a terminal.
	w.repair = gtk.NewButtonWithLabel("Start VPN service")
	w.repair.AddCSSClass("chip")
	w.repair.SetVisible(false)
	w.repair.ConnectClicked(func() { w.startDaemon() })
	card.Append(w.repair)

	card.Append(w.action)
	return card
}

// details builds the read-only rows: the three indicators worth being able to
// check at a glance.
func (w *window) details() *gtk.Widget {
	grid := gtk.NewGrid()
	grid.SetRowSpacing(8)
	grid.SetColumnSpacing(16)

	w.iface = gtk.NewLabel("—")
	w.ipv6 = gtk.NewLabel("—")
	w.ks = gtk.NewLabel("—")

	rows := []struct {
		key   string
		value *gtk.Label
	}{
		{"Interface", w.iface},
		{"IPv6", w.ipv6},
		{"Kill switch", w.ks},
	}
	for i, row := range rows {
		key := gtk.NewLabel(row.key)
		key.SetHAlign(gtk.AlignStart)
		// Muted, so the values carry the eye.
		key.AddCSSClass("muted-key")
		key.SetText(row.key)

		row.value.SetHAlign(gtk.AlignStart)
		row.value.SetSelectable(true)
		row.value.AddCSSClass("detail-value")

		grid.Attach(key, 0, i, 1, 1)
		grid.Attach(row.value, 1, i, 1, 1)
	}

	return &grid.Widget
}

func (w *window) present() { w.win.Present() }

// setBanner shows the strip across the top with text and one of the four state
// classes: connected, connecting, error, offline. An empty text hides it.
func (w *window) setBanner(text, kind string) {
	for _, class := range []string{"connecting", "error", "offline"} {
		w.banner.RemoveCSSClass(class)
	}
	if text == "" {
		w.banner.SetVisible(false)
		return
	}
	if kind != "" {
		w.banner.AddCSSClass(kind)
	}
	w.banner.SetText(text)
	w.banner.SetVisible(true)
}

// styleAction gives the one button its look for the state it is in: the accent
// gradient for Connect, a clean white for Disconnect — so the destructive
// action is never the most inviting thing in the card.
func (w *window) styleAction(connected bool) {
	if connected {
		w.action.RemoveCSSClass("connect-btn")
		w.action.AddCSSClass("disconnect-btn")
		return
	}
	w.action.RemoveCSSClass("disconnect-btn")
	w.action.AddCSSClass("connect-btn")
}

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
				// Stay on the main screen even mid-outage: the status card
				// carries the explanation and the button that starts the
				// service, which the login card has no room for.
				w.screens.SetVisibleChildName("main")
				w.accountBar.SetVisible(false)
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
	w.cityChoice = w.selectedCity()
	w.run(func(ctx context.Context) (*daemonclient.Status, error) {
		return w.client.Connect(ctx, daemonclient.Target{
			Country: w.selectedRegion(),
			City:    w.cityChoice,
			IPType:  w.selectedIPType(),
		})
	})
}

// selectedRegion is the country code currently chosen in the picker, or "" to
// let the API choose the fastest.
func (w *window) selectedRegion() string { return regionCodeAt(w.choices, int(w.region.Selected())) }

// selectedCity is the city slug currently chosen, or "" for any city in the
// country. Empty whenever the current country has no city list, so a picker
// left on a stale row from a previous country can never leak into a request.
func (w *window) selectedCity() string {
	code := w.selectedRegion()
	cities := regions.CitiesOf(w.places, code)
	if len(cities) == 0 {
		return ""
	}
	return regions.CitySlugAt(cities, int(w.city.Selected()))
}

// selectedCityName is the readable name of the city row, for the card.
func (w *window) selectedCityName() string {
	cities := regions.CitiesOf(w.places, w.selectedRegion())
	row := int(w.city.Selected())
	if row <= 0 || row > len(cities) {
		return ""
	}
	return cities[row-1].Name
}

// countryName prefers the name the account's own location list gave, which
// covers countries the built-in table does not know, then falls back to the
// table and finally to the raw code.
func (w *window) countryName(code string) string {
	for _, r := range w.choices {
		if strings.EqualFold(r.Code, code) {
			return r.Name
		}
	}
	return regions.Name(code)
}

// refreshLocationCard paints the card from the pickers, for the states where
// there is no live tunnel to report. When connected, apply writes the live
// values instead.
func (w *window) refreshLocationCard() {	if w.onScreen().State == daemonclient.StateConnected {
		return
	}
	code := w.selectedRegion()
	w.locFlag.SetText(flagEmoji(code))
	if code == "" {
		w.locName.SetText(fastestLabel)
	} else {
		w.locName.SetText(w.countryName(code))
	}
	meta := []string{}
	if city := w.selectedCityName(); city != "" {
		meta = append(meta, city)
	}
	meta = append(meta, ipTypeLabel(w.selectedIPType()))
	w.locMeta.SetText(strings.Join(meta, " · "))
	w.exitLine.SetText("Not connected")
	w.copy.SetSensitive(false)
}

// refreshCityPicker rebuilds the city dropdown's list for whatever country the
// region dropdown currently shows, and restores the remembered city choice
// within it if the new list still has it.
//
// Call this only when the underlying city data actually changes shape: a
// different country selected, or the daemon's location list arriving for the
// first time. It must NOT be called on every routine status poll — replacing
// the model resets the dropdown's selection, and if that ran on a timer it
// would silently discard whatever city the user had just clicked before the
// next tick landed, which is exactly the bug this split fixes.
//
// A city slug that does not belong to the selected country's list (the normal
// case right after picking a different country) simply is not found by
// CityRow, which returns row 0, "Any city" — so there is no need to clear
// cityChoice on a country change; it just stops matching anything until the
// user, or a synced connected status, sets it to one that does.
func (w *window) refreshCityPicker() {
	// SetModel resets the dropdown's row, which fires its notify handler and
	// overwrites cityChoice with "Any city". Take the wanted choice first and
	// put it back afterwards, so a choice made programmatically (a search hit)
	// survives the rebuild.
	want := w.cityChoice
	defer func() { w.cityChoice = want }()

	cities := regions.CitiesOf(w.places, w.selectedRegion())
	if len(cities) == 0 {
		w.city.SetModel(gtk.NewStringList([]string{"Any city"}))
		return
	}
	labels := make([]string, 0, len(cities)+1)
	labels = append(labels, "Any city")
	for _, c := range cities {
		labels = append(labels, c.Name)
	}
	w.city.SetModel(gtk.NewStringList(labels))
	w.city.SetSelected(regions.CityRow(cities, want))
}

// pickerEnabled is whether the region and city dropdowns may be used: while an
// operation of ours is in flight, or while a tunnel is actually up and changing
// region would have nothing to attach to.
func (w *window) pickerEnabled() bool {
	return !w.busy.Load() && w.onScreen().State != daemonclient.StateConnected
}

// applyCitySensitivity is the city dropdown's enabled state, recomputed on its
// own — safe to call on every poll, unlike refreshCityPicker, because it never
// touches the model or the selection, only whether the control can be used.
func (w *window) applyCitySensitivity() {
	w.city.SetSensitive(w.pickerEnabled() && len(regions.CitiesOf(w.places, w.selectedRegion())) > 0)

	w.ipType.SetSensitive(w.pickerEnabled())

	// Search follows the same rule as the pickers it drives, and needs the city
	// list to have arrived. When it goes inactive, drop any open results.
	searchable := w.pickerEnabled() && len(w.places) > 0
	w.search.SetSensitive(searchable)
	if !searchable && w.results.IsVisible() {
		w.clearSearch()
	}
}

// showSearchResults rebuilds the results list for the entry's current text.
func (w *window) showSearchResults() {
	for {
		row := w.results.RowAtIndex(0)
		if row == nil {
			break
		}
		w.results.Remove(row)
	}
	w.matches = regions.SearchCities(w.places, w.search.Text(), searchLimit)

	for _, m := range w.matches {
		label := gtk.NewLabel(flagEmoji(m.Code) + "  " + m.Label())
		label.SetHAlign(gtk.AlignStart)
		label.SetMarginTop(7)
		label.SetMarginBottom(7)
		label.SetMarginStart(12)
		label.SetMarginEnd(12)
		w.results.Append(label)
	}
	w.results.SetVisible(len(w.matches) > 0)
}

// clearSearch empties the entry and hides the results.
func (w *window) clearSearch() {
	w.search.SetText("") // fires search-changed, which empties the list
	w.matches = nil
	w.results.SetVisible(false)
}

// pickSearchResult selects a hit's country and city in the dropdowns.
func (w *window) pickSearchResult(m regions.CityMatch) {
	row := regionRow(w.choices, m.Code)
	if row == 0 {
		return // the country is not in the picker's list; do not guess
	}
	w.cityChoice = m.Slug
	w.region.SetSelected(row) // if the row changes, its handler rebuilds the cities
	w.refreshCityPicker()     // and if it did not, this does
	w.applyCitySensitivity()
	w.refreshLocationCard()
	w.clearSearch()
}

// loadPlaces pulls the country/city data for the selected IP type from the
// daemon. It mirrors loadCountries in every respect: off the main loop, one
// attempt at a time per type, asked for only until it succeeds, and silent on
// failure — the countries picker already works without it.
func (w *window) loadPlaces() {
	if w.shown.SignedOut() {
		return
	}
	t := w.selectedIPType()
	if _, ok := w.placesByType[t]; ok || w.loadingPlaces[t] {
		return
	}
	w.loadingPlaces[t] = true
	epoch := w.epoch

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		var list []regions.Place
		locs, err := w.client.Locations(ctx, t)
		if err != nil || len(locs) == 0 {
			log.Printf("locations(%s): countries=%d err=%v", t, len(locs), err)
		} else {
			log.Printf("locations(%s): got %d countries", t, len(locs))
			raw := make([]regions.RawCountry, 0, len(locs))
			for _, c := range locs {
				rc := regions.RawCountry{Code: c.Code, Name: c.Name}
				for _, ct := range c.Cities {
					rc.Cities = append(rc.Cities, regions.RawCity{Slug: ct.Slug, Name: ct.Name})
				}
				raw = append(raw, rc)
			}
			list = regions.FromLocations(raw)
		}

		w.onMain(func() {
			if w.epoch != epoch {
				return // asked for a previous account; its map was already replaced
			}
			delete(w.loadingPlaces, t)
			if len(list) == 0 {
				return
			}
			w.placesByType[t] = list
			if w.selectedIPType() != t {
				return // the user moved on; it is cached for when they return
			}
			w.showPlaces(list)
		})
	}()
}

// showPlaces makes a type's list the one on screen: its countries fill the
// region picker and its cities the city picker.
func (w *window) showPlaces(list []regions.Place) {
	w.places = list
	w.fillRegions(regions.Regions(list))
	w.refreshCityPicker()
	w.applyCitySensitivity()
	w.refreshLocationCard()
}

// onIPTypeChanged swaps the country and city lists for the newly selected
// type, using the cached list if there is one and fetching it if not.
func (w *window) onIPTypeChanged() {
	w.clearSearch()
	if list, ok := w.placesByType[w.selectedIPType()]; ok {
		w.showPlaces(list)
		return
	}
	// Not fetched yet: no cities (and no search) until it arrives. The country
	// list stays as it is meanwhile rather than emptying the picker.
	w.places = nil
	w.refreshCityPicker()
	w.applyCitySensitivity()
	w.refreshLocationCard()
	w.loadPlaces()
}

// selectedIPType is the API value of the IP type picker.
func (w *window) selectedIPType() string { return ipTypeAt(w.ipTypeIdx) }

// selectIPType moves the picker to a type reported by the daemon's status, so a
// connected window shows the pool actually in use.
func (w *window) selectIPType(value string) {
	if row := int(ipTypeRow(value)); row != w.ipTypeIdx {
		w.ipType.SetCurrentPage(row) // fires the switch-page handler
	}
}

// ipTypes are the tabs. Values are what the API takes: it calls the data centre
// pool "hosting", while the app offers it as the high-speed pool, which is what
// it is for.
var ipTypes = []struct{ Value, Label string }{
	{"residential", "Residential"},
	{"hosting", "High-speed"},
}

func ipTypeLabels() []string {
	out := make([]string, len(ipTypes))
	for i, t := range ipTypes {
		out[i] = t.Label
	}
	return out
}

// ipTypeAt maps a tab index to an API value, and ipTypeRow does the reverse.
// A fresh dropdown reports -1 and an unknown or empty type (which the daemon
// treats as residential) maps to row 0, so neither can select the wrong pool.
func ipTypeAt(row int) string {
	if row < 0 || row >= len(ipTypes) {
		return ipTypes[0].Value
	}
	return ipTypes[row].Value
}

func ipTypeRow(value string) uint {
	for i, t := range ipTypes {
		if strings.EqualFold(t.Value, value) {
			return uint(i)
		}
	}
	return 0
}

// ipTypeLabel is a type's readable name for the status line.
func ipTypeLabel(value string) string {
	for _, t := range ipTypes {
		if strings.EqualFold(t.Value, value) {
			return t.Label
		}
	}
	return value
}

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

// pickerLabels is the dropdown's row labels: "Fastest", then the country names
// with their flags.
func (w *window) pickerLabels() []string {
	names := make([]string, 0, len(w.choices)+1)
	names = append(names, "\U0001F310  "+fastestLabel)
	for _, r := range w.choices {
		names = append(names, flagEmoji(r.Code)+"  "+r.Name)
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
	w.notice = ""
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
	wasOut := w.shown.SignedOut()
	w.shown = st

	// No account means no tunnel to show and nothing to pick: the login screen
	// stands in for the whole main screen.
	if st.SignedOut() {
		// Shown before it is reset: focus can only land on a widget that is on
		// screen.
		w.screens.SetVisibleChildName("login")
		w.accountBar.SetVisible(false)
		w.repair.SetVisible(false)
		w.setBanner("", "")
		w.statusVisual.setState("disconnected")
		if !wasOut {
			w.onAccountChanged()
			w.showLoginEmail()
		}
		return
	}
	if wasOut {
		// Someone just signed in. Whatever the pickers held belonged to nobody
		// now, and they refill from the new account on the next poll.
		w.onAccountChanged()
	}
	w.screens.SetVisibleChildName("main")

	// Shown only when the daemon spoke to accounts at all; an older one says
	// nothing, and a Sign out button for it would have nothing to call.
	w.accountBar.SetVisible(st.LoggedIn != nil)
	if st.Account != "" {
		w.account.SetText(st.Account)
	} else {
		w.account.SetText("Signed in")
	}

	w.repair.SetVisible(false)

	switch st.State {
	case daemonclient.StateConnected:
		w.heading.SetMarkup("<b>Connected</b>")
		w.detail.SetText(w.regionLine(st))
		w.setBanner("●  Connected", "connected")
		w.statusVisual.setState(daemonclient.StateConnected)
	case daemonclient.StateConnecting:
		w.heading.SetMarkup("<b>Connecting…</b>")
		w.detail.SetText("Bringing the tunnel up. This can take a moment.")
		w.setBanner("●  Connecting…", "connecting")
		w.statusVisual.setState(daemonclient.StateConnecting)
	case daemonclient.StateReconnecting:
		w.heading.SetMarkup("<b>Reconnecting…</b>")
		w.detail.SetText(w.reconnectLine(st))
		w.setBanner("●  Reconnecting…", "connecting")
		w.statusVisual.setState(daemonclient.StateReconnecting)
	case daemonclient.StateError:
		w.heading.SetMarkup("<b>Connection failed</b>")
		w.detail.SetText(w.idleLine(st))
		w.setBanner("●  Disconnected", "error")
		w.statusVisual.setState(daemonclient.StateError)
	default:
		w.heading.SetMarkup("<b>Disconnected</b>")
		w.detail.SetText(w.idleLine(st))
		w.setBanner("●  Disconnected", "error")
		w.statusVisual.setState(daemonclient.StateDisconnected)
	}
	if w.notice != "" {
		w.detail.SetText(w.notice)
	}
	w.styleAction(st.State == daemonclient.StateConnected)

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
	regionEnabled := !busy && st.State != daemonclient.StateConnected
	w.region.SetSensitive(regionEnabled)
	if st.State == daemonclient.StateConnected {
		// Set before selectRegion: if that actually changes the picker's row it
		// fires the region's own notify handler, which rebuilds the city list
		// from whatever cityChoice already holds — so it has to be the
		// connected city by that point, not the choice from before connecting.
		w.cityChoice = st.City
		// The type first: changing it swaps the country list the next line
		// selects within.
		w.selectIPType(st.IPType)
		w.selectRegion(st.Country)

		// The card shows the live tunnel, not the picker's intent.
		w.locFlag.SetText(flagEmoji(st.Country))
		if st.Country == "" {
			w.locName.SetText("Connected")
		} else {
			w.locName.SetText(w.countryName(st.Country))
		}
		meta := []string{}
		if st.City != "" {
			meta = append(meta, regions.CityName(w.places, st.Country, st.City))
		}
		if st.IPType != "" {
			meta = append(meta, ipTypeLabel(st.IPType))
		}
		w.locMeta.SetText(strings.Join(meta, " · "))
		if st.ExitIP != "" {
			w.exitLine.SetText("Exit IP  " + st.ExitIP)
			w.copy.SetSensitive(true)
		} else {
			w.exitLine.SetText("Exit IP  —")
			w.copy.SetSensitive(false)
		}
	} else {
		w.refreshLocationCard()
	}
	w.applyCitySensitivity()

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
	if w.haveCountries.Load() || w.shown.SignedOut() {
		return // already have them, or nobody to ask on behalf of
	}
	if !w.loadingCountries.CompareAndSwap(false, true) {
		return // one attempt in flight is enough
	}
	epoch := w.epoch
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
			if w.epoch != epoch {
				return // asked for a previous account
			}
			if w.haveCountries.Swap(true) {
				return // a slower earlier request beat us to it
			}
			if len(w.places) > 0 {
				return // the selected type's own list is already on screen
			}
			w.fillRegions(list)
			w.refreshLocationCard()
		})
	}()
}

// showUnreachable is for a daemon that did not answer at all, which is a
// different problem from a daemon that answered with a failure.
func (w *window) showUnreachable(err error) {
	w.action.SetSensitive(false)
	w.action.SetLabel("Connect")
	w.styleAction(false)
	w.setBanner("●  VPN service offline", "offline")
	w.statusVisual.setState("error")
	w.heading.SetMarkup("<b>VPN service is not running</b>")
	w.detail.SetText("Start the service below, then try again.\n\n" + err.Error())
	w.iface.SetText("—")
	w.ipv6.SetText("—")
	w.ks.SetText("—")
	w.exitLine.SetText("Not connected")
	w.copy.SetSensitive(false)
	w.locFlag.SetText("\U0001F310")
	w.locName.SetText("No location yet")
	w.locMeta.SetText("Start the service to choose a location")
	w.repair.SetVisible(true)
	w.region.SetSensitive(false)
	w.city.SetSensitive(false)
	w.ipType.SetSensitive(false)
	w.search.SetSensitive(false)
}

// startDaemon asks the service manager (or, when there is no service, pkexec)
// to bring the privileged daemon up, then resumes polling. It is the graphical
// equivalent of "sudo systemctl start mystd" for someone who should not have to
// know that command.
func (w *window) startDaemon() {
	if !w.busy.CompareAndSwap(false, true) {
		return
	}
	w.repair.SetSensitive(false)
	w.repair.SetLabel("Starting…")

	go func() {
		err := ensureDaemon(w.socket)
		w.onMain(func() {
			w.busy.Store(false)
			w.repair.SetSensitive(true)
			w.repair.SetLabel("Start VPN service")
			if err != nil {
				w.heading.SetMarkup("<b>Could not start the VPN service</b>")
				w.detail.SetText(err.Error())
				return
			}
			w.poll()
		})
	}()
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
		w.copy.SetLabel("Copy IP")
		return false // one shot, not a repeating timer
	})
}

func (w *window) regionLine(st daemonclient.Status) string {
	name := regions.Name(st.Country)
	if st.City != "" {
		name += " · " + regions.CityName(w.places, st.Country, st.City)
	}
	if st.IPType != "" {
		name += " · " + ipTypeLabel(st.IPType)
	}
	return "Your traffic is encrypted through " + name + "."
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
	return "Your traffic is going out directly. Choose a location and connect."
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
