package main

import (
	"log"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Menu item ids. Region entries are allocated out of their own block by
// idForRegion, which is what keeps an id attached to one country for the life
// of the process.
const (
	idStatus     = 1
	idSepTop     = 2
	idConnect    = 3
	idDisconnect = 4

	idRegion     = 10
	idRegionBase = 100

	idCopyIP = 20
	idSepEnd = 21
	idQuit   = 22
)

// idForRegion returns the menu id for a country, allocating one the first time
// it is seen. Caller holds a.mu.
//
// The id belongs to the country, not to its row, and that is the whole point.
// The list changes once in the applet's life — from the built-in fallback to
// the account's real one — and the two are in different orders: the fallback is
// hand-ordered by popularity, the real list is sorted by display name. A
// row-indexed id would name a different country after that swap, so a click
// that raced the panel's re-read would connect somewhere the user did not
// choose. An id handed to exactly one country cannot do that.
//
// Ids are never reused, so the mapping only grows. That costs a handful of ints
// for a list that changes at most twice.
func (a *app) idForRegion(code string) int32 {
	code = strings.ToUpper(strings.TrimSpace(code))
	if id, ok := a.regionIDs[code]; ok {
		return id
	}
	id := a.nextRegionID
	a.nextRegionID++
	a.regionIDs[code] = id
	a.regionCodes[id] = code
	return id
}

// regionForID is the reverse, used when a click comes in. The bool reports
// whether the id names a country at all, so a stale click from a panel that has
// not caught up is ignored rather than guessed at.
//
// It takes the lock itself: it is called from the D-Bus Event handler, while a
// GetLayout on another goroutine may be adding ids to the same map, and an
// unsynchronised read of a map being written is a crash, not a stale answer.
func (a *app) regionForID(id int32) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	code, ok := a.regionCodes[id]
	return code, ok
}

// cityTarget is a country and one of its cities, which is what a click on a
// city entry means.
type cityTarget struct{ country, city string }

// idForCity returns the menu id for a city, allocating one the first time it is
// seen. Caller holds a.mu.
//
// Keyed by country and slug, never by row, for the reason idForRegion is: the
// list can change under a panel that has not re-read it, and an id that named a
// position would then name a different city. The slug is used exactly as the
// API spelled it, because the API matches it literally.
func (a *app) idForCity(code, slug string) int32 {
	code = strings.ToUpper(strings.TrimSpace(code))
	key := code + "/" + slug
	if id, ok := a.cityIDs[key]; ok {
		return id
	}
	id := a.nextRegionID
	a.nextRegionID++
	a.cityIDs[key] = id
	a.cityTargets[id] = cityTarget{country: code, city: slug}
	return id
}

// idForGroup returns the id of the submenu that holds a country's cities.
// Caller holds a.mu. Deliberately not reversible: a parent is not a choice, and
// a host that reports a click on one gets nothing rather than a guess.
func (a *app) idForGroup(code string) int32 {
	code = strings.ToUpper(strings.TrimSpace(code))
	if id, ok := a.groupIDs[code]; ok {
		return id
	}
	id := a.nextRegionID
	a.nextRegionID++
	a.groupIDs[code] = id
	return id
}

// cityForID is regionForID's counterpart for city entries.
func (a *app) cityForID(id int32) (cityTarget, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.cityTargets[id]
	return t, ok
}

// layoutItem is dbusmenu's (ia{sv}av): an id, its properties, and its children
// as variants so the recursion can nest.
type layoutItem struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}

// groupProps is one element of GetGroupProperties' a(ia{sv}) answer.
type groupProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

// menuMethods implements com.canonical.dbusmenu.
type menuMethods struct{ a *app }

func (m *menuMethods) GetLayout(parentID int32, depth int32, names []string) (uint32, layoutItem, *dbus.Error) {
	a := m.a
	a.mu.Lock()
	defer a.mu.Unlock()

	root := m.buildLocked()
	rev := a.rev

	if parentID != 0 {
		item, ok := findItem(root, parentID)
		if !ok {
			// A stale id from a host that has not caught up with a layout
			// change. An empty node is the graceful answer; an error makes
			// some hosts drop the menu entirely.
			return rev, layoutItem{ID: parentID, Properties: map[string]dbus.Variant{}}, nil
		}
		root = item
	}
	if depth == 0 {
		root.Children = nil
	}
	return rev, root, nil
}

func (m *menuMethods) GetGroupProperties(ids []int32, names []string) ([]groupProps, *dbus.Error) {
	a := m.a
	a.mu.Lock()
	defer a.mu.Unlock()

	root := m.buildLocked()

	out := make([]groupProps, 0, len(ids))
	for _, id := range ids {
		item, ok := findItem(root, id)
		if !ok {
			continue
		}
		out = append(out, groupProps{ID: id, Props: item.Properties})
	}
	return out, nil
}

func (m *menuMethods) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	a := m.a
	a.mu.Lock()
	defer a.mu.Unlock()

	item, ok := findItem(m.buildLocked(), id)
	if !ok {
		return dbus.MakeVariant(""), unknownProperty(name)
	}
	v, ok := item.Properties[name]
	if !ok {
		return dbus.MakeVariant(""), unknownProperty(name)
	}
	return v, nil
}

func (m *menuMethods) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}

	a := m.a
	switch {
	case id == idConnect:
		a.doConnect("")
	case id == idDisconnect:
		a.doDisconnect()
	case id == idCopyIP:
		if !a.copyExitIP() {
			log.Print("could not copy the exit IP: no working clipboard tool")
		}
	case id == idQuit:
		a.quitNow()
	default:
		// Regions are found by looking the id up rather than by subtracting a
		// base, because a click can arrive for a country the menu no longer
		// lists — the panel may not have re-read the layout yet.
		if code, ok := a.regionForID(id); ok {
			a.doConnect(code)
		} else if t, ok := a.cityForID(id); ok {
			a.connectTo(t.country, t.city)
		}
	}
	return nil
}

// AboutToShow lets a host ask for a lazy rebuild. This applet pushes updates
// instead, so there is never anything to recompute on demand.
func (m *menuMethods) AboutToShow(id int32) (bool, *dbus.Error) { return false, nil }

func (m *menuMethods) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

// buildLocked assembles the whole menu. Caller holds a.mu.
func (m *menuMethods) buildLocked() layoutItem {
	a := m.a

	connected := a.st.State == stateConnected
	connecting := a.st.State == stateConnecting || a.st.State == stateReconnecting
	busy := a.busy || connecting

	root := layoutItem{ID: 0, Properties: map[string]dbus.Variant{}, Children: []dbus.Variant{}}
	add := func(it layoutItem) {
		root.Children = append(root.Children, dbus.MakeVariant(it))
	}

	// A disabled first line that says what is going on. It is the only place
	// the exit IP appears without hovering, and the only place an error message
	// surfaces at all.
	add(layoutItem{ID: idStatus, Properties: disabled(statusLine(a))})
	add(layoutItem{ID: idSepTop, Properties: separatorProps()})

	add(layoutItem{ID: idConnect, Properties: enabled("Connect", !busy && !connected)})
	add(layoutItem{ID: idDisconnect, Properties: enabled("Disconnect", !busy && connected)})

	// Region submenu, with the current one checked. The list is whatever the
	// daemon last supplied, which is the account's own — the built-in fallback
	// only until that first answer arrives.
	sub := layoutItem{ID: idRegion, Properties: submenuProps("Region"), Children: []dbus.Variant{}}
	radio := func(label string, checked bool) map[string]dbus.Variant {
		props := enabled(label, !busy)
		props["toggle-type"] = dbus.MakeVariant("radio")
		state := int32(0)
		if checked {
			state = 1
		}
		props["toggle-state"] = dbus.MakeVariant(state)
		return props
	}
	for _, r := range a.choices {
		chosen := strings.EqualFold(r.Code, a.country)
		cities := citiesOf(a.places, r.Code)

		if len(cities) == 0 {
			// No cities to choose between, or none known yet: the country is
			// the whole choice, as it was before cities existed.
			sub.Children = append(sub.Children, dbus.MakeVariant(layoutItem{
				ID:         a.idForRegion(r.Code),
				Properties: radio(r.Name, chosen),
			}))
			continue
		}

		// A country with cities is a submenu, with "Any city" first. That entry
		// is the country on its own and reuses the country's id, so a click on
		// it means exactly what a click on the plain entry always meant.
		//
		// A submenu parent cannot carry a radio mark of its own, so the chosen
		// country is marked in its label instead; otherwise the user would have
		// to open every country to find out which one they were on.
		label := r.Name
		if chosen {
			label += " ✓"
		}
		group := layoutItem{
			ID:         a.idForGroup(r.Code),
			Properties: submenuProps(label),
			Children:   []dbus.Variant{},
		}
		group.Children = append(group.Children, dbus.MakeVariant(layoutItem{
			ID:         a.idForRegion(r.Code),
			Properties: radio("Any city", chosen && a.city == ""),
		}))
		for _, c := range cities {
			group.Children = append(group.Children, dbus.MakeVariant(layoutItem{
				ID:         a.idForCity(r.Code, c.Slug),
				Properties: radio(c.Name, chosen && strings.EqualFold(a.city, c.Slug)),
			}))
		}
		sub.Children = append(sub.Children, dbus.MakeVariant(group))
	}
	add(sub)

	add(layoutItem{ID: idSepEnd, Properties: separatorProps()})

	canCopy := connected && a.st.ExitIP != "" && clipboardTool()
	add(layoutItem{ID: idCopyIP, Properties: enabled("Copy exit IP", canCopy)})

	add(layoutItem{ID: idQuit, Properties: enabled("Quit", true)})

	return root
}

// statusLine is the human-readable summary shown at the top of the menu.
func statusLine(a *app) string {
	switch {
	case !a.reachable:
		return "Daemon not reachable"
	case a.st.State == stateConnected:
		line := "Connected"
		if a.st.Country != "" {
			line += " — " + regionName(a.st.Country)
			if a.st.City != "" {
				line += " · " + cityName(a.places, a.st.Country, a.st.City)
			}
		}
		if a.st.ExitIP != "" {
			line += " (" + a.st.ExitIP + ")"
		}
		return line
	case a.st.State == stateConnecting:
		return "Connecting…"
	case a.st.State == stateReconnecting:
		// The daemon noticed the tunnel die and is repairing it on its own.
		// Saying so matters: without it the user sees a connected-looking
		// client and a network that does not work.
		line := "Reconnecting…"
		if a.st.LastError != "" {
			line += " (" + a.st.LastError + ")"
		}
		return line
	case a.st.State == stateError:
		if a.st.LastError != "" {
			return "Error — " + a.st.LastError
		}
		return "Error"
	default:
		return "Not connected"
	}
}

func findItem(root layoutItem, id int32) (layoutItem, bool) {
	if root.ID == id {
		return root, true
	}
	for _, child := range root.Children {
		item, ok := child.Value().(layoutItem)
		if !ok {
			continue
		}
		if found, ok := findItem(item, id); ok {
			return found, true
		}
	}
	return layoutItem{}, false
}

func baseProps(label string, isEnabled bool) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"label":   dbus.MakeVariant(label),
		"enabled": dbus.MakeVariant(isEnabled),
		"visible": dbus.MakeVariant(true),
	}
}

func enabled(label string, isEnabled bool) map[string]dbus.Variant {
	return baseProps(label, isEnabled)
}

func disabled(label string) map[string]dbus.Variant {
	return baseProps(label, false)
}

func submenuProps(label string) map[string]dbus.Variant {
	p := baseProps(label, true)
	p["children-display"] = dbus.MakeVariant("submenu")
	return p
}

func separatorProps() map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"type":    dbus.MakeVariant("separator"),
		"visible": dbus.MakeVariant(true),
	}
}

// menuProps implements org.freedesktop.DBus.Properties for the menu object.
type menuProps struct{ a *app }

func (p *menuProps) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	all, err := p.GetAll(iface)
	if err != nil {
		return dbus.MakeVariant(""), err
	}
	v, ok := all[name]
	if !ok {
		return dbus.MakeVariant(""), unknownProperty(name)
	}
	return v, nil
}

func (p *menuProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != menuInterface {
		return nil, unknownInterface(iface)
	}
	return map[string]dbus.Variant{
		"Version":       dbus.MakeVariant(uint32(3)),
		"TextDirection": dbus.MakeVariant("ltr"),
		"Status":        dbus.MakeVariant("normal"),
		"IconThemePath": dbus.MakeVariant([]string{}),
	}, nil
}

func (p *menuProps) Set(iface, name string, value dbus.Variant) *dbus.Error {
	return readOnly(name)
}

const menuIntrospectionXML = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
  <interface name="com.canonical.dbusmenu">
    <method name="GetLayout">
      <arg name="parentId" type="i" direction="in"/>
      <arg name="recursionDepth" type="i" direction="in"/>
      <arg name="propertyNames" type="as" direction="in"/>
      <arg name="revision" type="u" direction="out"/>
      <arg name="layout" type="(ia{sv}av)" direction="out"/>
    </method>
    <method name="GetGroupProperties">
      <arg name="ids" type="ai" direction="in"/>
      <arg name="propertyNames" type="as" direction="in"/>
      <arg name="properties" type="a(ia{sv})" direction="out"/>
    </method>
    <method name="GetProperty">
      <arg name="id" type="i" direction="in"/>
      <arg name="name" type="s" direction="in"/>
      <arg name="value" type="v" direction="out"/>
    </method>
    <method name="Event">
      <arg name="id" type="i" direction="in"/>
      <arg name="eventId" type="s" direction="in"/>
      <arg name="data" type="v" direction="in"/>
      <arg name="timestamp" type="u" direction="in"/>
    </method>
    <method name="AboutToShow"><arg name="id" type="i" direction="in"/><arg name="needUpdate" type="b" direction="out"/></method>
    <method name="AboutToShowGroup">
      <arg name="ids" type="ai" direction="in"/>
      <arg name="updatesNeeded" type="ai" direction="out"/>
      <arg name="idErrors" type="ai" direction="out"/>
    </method>
    <signal name="LayoutUpdated"><arg name="revision" type="u"/><arg name="parent" type="i"/></signal>
    <signal name="ItemsPropertiesUpdated">
      <arg name="updatedProps" type="a(ia{sv})"/>
      <arg name="removedProps" type="a(ias)"/>
    </signal>
    <property name="Version" type="u" access="read"/>
    <property name="TextDirection" type="s" access="read"/>
    <property name="Status" type="s" access="read"/>
    <property name="IconThemePath" type="as" access="read"/>
  </interface>
  <interface name="org.freedesktop.DBus.Properties">
    <method name="Get">
      <arg name="interface" type="s" direction="in"/>
      <arg name="property" type="s" direction="in"/>
      <arg name="value" type="v" direction="out"/>
    </method>
    <method name="GetAll">
      <arg name="interface" type="s" direction="in"/>
      <arg name="properties" type="a{sv}" direction="out"/>
    </method>
    <method name="Set">
      <arg name="interface" type="s" direction="in"/>
      <arg name="property" type="s" direction="in"/>
      <arg name="value" type="v" direction="in"/>
    </method>
  </interface>
  <interface name="org.freedesktop.DBus.Introspectable">
    <method name="Introspect"><arg name="xml" type="s" direction="out"/></method>
  </interface>
</node>`
