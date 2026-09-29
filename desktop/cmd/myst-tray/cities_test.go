package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/zamirszn/mysterium-linux-client/desktop/internal/daemonclient"
)

// citiesFake is a daemon with a country list and cities for two of the three
// countries in it. Germany is listed without cities, as the real API does for
// some countries, so the plain-entry path is covered by the same fixture.
func citiesFake() *fakeClient {
	return &fakeClient{
		statusResp:    &status{State: stateDisconnected},
		connectResp:   &status{State: stateConnected},
		countriesResp: []string{"GB", "DE", "US"},
		locationsResp: []daemonclient.LocationCountry{
			{Code: "GB", Name: "United Kingdom", Cities: []daemonclient.LocationCity{
				{Slug: "manchester", Name: "Manchester"},
				{Slug: "london", Name: "London"},
			}},
			{Code: "DE", Name: "Germany"},
			{Code: "US", Name: "United States", Cities: []daemonclient.LocationCity{
				{Slug: "new-york", Name: "New York"},
			}},
		},
	}
}

// loaded returns an app that has heard the daemon's country and city lists.
func loaded(t *testing.T, c *fakeClient, country string) (*app, *menuMethods) {
	t.Helper()
	a, _ := newTestApp(c, country)
	a.loadCountries(context.Background())
	a.loadPlaces(context.Background())
	waitFor(t, "the country and city lists", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.haveCountries && a.havePlaces
	})
	return a, &menuMethods{a}
}

func childLabels(t *testing.T, it layoutItem) []string {
	t.Helper()
	var out []string
	for _, ch := range it.Children {
		c := ch.Value().(layoutItem)
		out = append(out, stringProp(t, c.Properties, "label"))
	}
	return out
}

func toggle(t *testing.T, root layoutItem, id int32) int32 {
	t.Helper()
	v, ok := itemProps(t, root, id)["toggle-state"].Value().(int32)
	if !ok {
		t.Fatalf("item %d has no integer toggle-state", id)
	}
	return v
}

func TestACountryWithCitiesBecomesASubmenuWithAnyCityFirst(t *testing.T) {
	a, m := loaded(t, citiesFake(), "")
	root := m.buildLocked()

	a.mu.Lock()
	group := a.groupIDs["GB"]
	a.mu.Unlock()

	it, ok := findItem(root, group)
	if !ok {
		t.Fatal("United Kingdom has cities but the menu has no submenu for it")
	}
	if got := stringProp(t, it.Properties, "children-display"); got != "submenu" {
		t.Errorf("children-display = %q, want submenu", got)
	}
	// "Any city" first, then the cities sorted by name whatever order the API used.
	want := []string{"Any city", "London", "Manchester"}
	if got := childLabels(t, it); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("submenu = %v, want %v", got, want)
	}
}

func TestACountryWithoutCitiesStaysAPlainEntry(t *testing.T) {
	a, m := loaded(t, citiesFake(), "")
	root := m.buildLocked()

	a.mu.Lock()
	_, hasGroup := a.groupIDs["DE"]
	id := a.regionIDs["DE"]
	a.mu.Unlock()

	if hasGroup {
		t.Error("Germany has no cities and must not become a submenu")
	}
	if _, isSub := itemProps(t, root, id)["children-display"]; isSub {
		t.Error("Germany's entry is a submenu")
	}
	if got := stringProp(t, itemProps(t, root, id), "label"); got != "Germany" {
		t.Errorf("label = %q, want Germany", got)
	}
}

func TestClickingACityConnectsToThatCityInThatCountry(t *testing.T) {
	c := citiesFake()
	a, m := loaded(t, c, "")
	m.buildLocked()

	a.mu.Lock()
	id := a.cityIDs["GB/london"]
	a.mu.Unlock()
	if id == 0 {
		t.Fatal("the menu gave London no id")
	}

	if err := m.Event(id, "clicked", dbus.MakeVariant(""), 0); err != nil {
		t.Fatalf("Event: %v", err)
	}
	waitFor(t, "the connect to reach the daemon", func() bool { _, n := c.counts(); return n == 1 })

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastCountry != "GB" || c.lastCity != "london" {
		t.Errorf("connected to %q/%q, want GB/london — the slug must reach the daemon verbatim", c.lastCountry, c.lastCity)
	}
}

func TestAnyCityConnectsToTheCountryAlone(t *testing.T) {
	c := citiesFake()
	a, m := loaded(t, c, "")
	m.buildLocked()

	// Pick London first so there is a remembered city to be cleared.
	a.connectTo("GB", "london")
	waitFor(t, "first connect", func() bool { _, _, busy := a.state(); return !busy })

	a.mu.Lock()
	any := a.regionIDs["GB"]
	a.mu.Unlock()
	m.Event(any, "clicked", dbus.MakeVariant(""), 0)
	waitFor(t, "second connect", func() bool { _, n := c.counts(); return n == 2 })

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastCountry != "GB" || c.lastCity != "" {
		t.Errorf("Any city connected to %q/%q, want GB with no city", c.lastCountry, c.lastCity)
	}
}

// TestChoosingAnotherCountryDropsTheCity: a city belongs to the country it was
// picked in. Carrying "london" into Germany would ask for a place that does not
// exist and fail with an error that reads like Germany being out of servers.
func TestChoosingAnotherCountryDropsTheCity(t *testing.T) {
	c := citiesFake()
	a, _ := loaded(t, c, "")

	a.connectTo("GB", "london")
	waitFor(t, "first connect", func() bool { _, _, busy := a.state(); return !busy })
	a.connectTo("DE", "")
	waitFor(t, "second connect", func() bool { _, n := c.counts(); return n == 2 })
	waitFor(t, "second connect to settle", func() bool { _, _, busy := a.state(); return !busy })

	c.mu.Lock()
	gotCountry, gotCity := c.lastCountry, c.lastCity
	c.mu.Unlock()
	if gotCountry != "DE" || gotCity != "" {
		t.Errorf("connected to %q/%q, want DE with no city", gotCountry, gotCity)
	}
	a.mu.Lock()
	remembered := a.city
	a.mu.Unlock()
	if remembered != "" {
		t.Errorf("the applet still remembers city %q after switching country", remembered)
	}
}

// TestTheIconClickReusesTheRememberedCity: an empty country means "what I
// chose last", and that includes the city.
func TestTheIconClickReusesTheRememberedCity(t *testing.T) {
	c := citiesFake()
	a, _ := loaded(t, c, "")

	a.connectTo("GB", "manchester")
	waitFor(t, "first connect", func() bool { _, _, busy := a.state(); return !busy })

	a.doConnect("")
	waitFor(t, "second connect", func() bool { _, n := c.counts(); return n == 2 })

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastCountry != "GB" || c.lastCity != "manchester" {
		t.Errorf("reconnect went to %q/%q, want GB/manchester", c.lastCountry, c.lastCity)
	}
}

func TestTheMenuChecksTheChosenCityAndMarksItsCountry(t *testing.T) {
	c := citiesFake()
	a, m := loaded(t, c, "gb")

	root := m.buildLocked()
	a.mu.Lock()
	any, group := a.regionIDs["GB"], a.groupIDs["GB"]
	a.mu.Unlock()

	// Country chosen, no city: "Any city" is the checked entry.
	if toggle(t, root, any) != 1 {
		t.Error("Any city is not checked when the country is chosen without a city")
	}
	if got := stringProp(t, itemProps(t, root, group), "label"); got != "United Kingdom ✓" {
		t.Errorf("submenu label = %q, want the chosen country marked", got)
	}

	a.mutate(func() { a.city = "LONDON" }) // casing must not matter
	root = m.buildLocked()
	a.mu.Lock()
	london := a.cityIDs["GB/london"]
	manchester := a.cityIDs["GB/manchester"]
	a.mu.Unlock()

	if toggle(t, root, london) != 1 || toggle(t, root, manchester) != 0 || toggle(t, root, any) != 0 {
		t.Error("exactly London should be checked once London is the chosen city")
	}

	// A country that is not the chosen one is not marked, and none of its
	// entries are checked even if the remembered city's slug happens to match.
	a.mu.Lock()
	usGroup := a.groupIDs["US"]
	a.mu.Unlock()
	if got := stringProp(t, itemProps(t, root, usGroup), "label"); got != "United States" {
		t.Errorf("unchosen country label = %q, want no mark", got)
	}
}

// TestCityIDsFollowTheCityNotTheRow is TestRegionIDsFollowTheCountryNotTheRow
// for cities: a click on an id the panel last saw must still mean that city
// after the list is re-sorted underneath it.
func TestCityIDsFollowTheCityNotTheRow(t *testing.T) {
	c := citiesFake()
	a, m := loaded(t, c, "")
	m.buildLocked()

	a.mu.Lock()
	london := a.cityIDs["GB/london"]
	a.mu.Unlock()

	// The city list changes shape and order.
	a.mutate(func() {
		a.places = fromLocations([]daemonclient.LocationCountry{{Code: "GB", Cities: []daemonclient.LocationCity{
			{Slug: "aberdeen", Name: "Aberdeen"},
			{Slug: "london", Name: "London"},
			{Slug: "york", Name: "York"},
		}}})
		a.choicesSeq++
	})
	m.buildLocked()

	a.mu.Lock()
	now := a.cityIDs["GB/london"]
	a.mu.Unlock()
	if now != london {
		t.Fatalf("London's id changed from %d to %d when the list did", london, now)
	}
	if target, ok := a.cityForID(london); !ok || target.country != "GB" || target.city != "london" {
		t.Errorf("id %d resolves to %+v, want GB/london", london, target)
	}
}

func TestAClickOnASubmenuParentConnectsNowhere(t *testing.T) {
	c := citiesFake()
	a, m := loaded(t, c, "")
	m.buildLocked()

	a.mu.Lock()
	group := a.groupIDs["GB"]
	a.mu.Unlock()
	m.Event(group, "clicked", dbus.MakeVariant(""), 0)

	time.Sleep(50 * time.Millisecond)
	if _, n := c.counts(); n != 0 {
		t.Errorf("clicking a submenu parent started %d connects, want none", n)
	}
}

func TestCitiesAreAskedForUntilTheyArriveThenNotAgain(t *testing.T) {
	c := &fakeClient{statusResp: &status{State: stateDisconnected}, locationsErr: context.DeadlineExceeded}
	a, _ := newTestApp(c, "")

	a.loadPlaces(context.Background())
	waitFor(t, "first attempt to finish", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.loadingPlaces
	})
	a.loadPlaces(context.Background())
	waitFor(t, "second attempt", func() bool { return c.askedForLocations() >= 2 })

	// A daemon that cannot serve cities leaves every country a plain entry.
	m := &menuMethods{a}
	m.buildLocked()
	a.mu.Lock()
	groups := len(a.groupIDs)
	a.mu.Unlock()
	if groups != 0 {
		t.Errorf("%d submenus built with no city list, want none", groups)
	}

	c.mu.Lock()
	c.locationsErr = nil
	c.locationsResp = citiesFake().locationsResp
	c.mu.Unlock()
	a.loadPlaces(context.Background())
	waitFor(t, "the list to arrive", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.havePlaces
	})

	before := c.askedForLocations()
	a.loadPlaces(context.Background())
	time.Sleep(50 * time.Millisecond)
	if after := c.askedForLocations(); after != before {
		t.Errorf("asked for cities again after having them (%d → %d)", before, after)
	}
}

func TestTheCityListChangingRevisesTheMenu(t *testing.T) {
	c := citiesFake()
	a, rec := newTestApp(c, "")
	a.loadPlaces(context.Background())
	waitFor(t, "the list to arrive", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.havePlaces
	})
	found := false
	for _, s := range rec.take() {
		if strings.HasSuffix(s, ".LayoutUpdated") {
			found = true
		}
	}
	if !found {
		t.Error("cities arrived but the panel was never told to re-read the menu")
	}
}

func TestStatusLineAndTooltipNameTheCity(t *testing.T) {
	c := citiesFake()
	a, _ := loaded(t, c, "")

	a.mutate(func() {
		a.reachable = true
		a.st = status{State: stateConnected, Country: "GB", City: "st-albans", ExitIP: "203.0.113.7"}
	})
	a.mu.Lock()
	line, tip := statusLine(a), a.tooltip
	a.mu.Unlock()

	if !strings.Contains(line, "United Kingdom · St-Albans") {
		t.Errorf("status line %q does not name the city", line)
	}
	if !strings.Contains(tip, "St-Albans") {
		t.Errorf("tooltip %q does not name the city", tip)
	}
}

func TestTheIdleTooltipNamesWhereAClickWillGo(t *testing.T) {
	c := citiesFake()
	a, _ := loaded(t, c, "gb")
	a.mutate(func() {
		a.reachable = true
		a.st = status{State: stateDisconnected}
		a.city = "london"
	})
	a.mu.Lock()
	tip := a.tooltip
	a.mu.Unlock()
	if !strings.Contains(tip, "London, United Kingdom") {
		t.Errorf("tooltip %q does not say a click connects to London, United Kingdom", tip)
	}
}
