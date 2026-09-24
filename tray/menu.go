package main

import (
	"log"

	"github.com/godbus/dbus/v5"
)

// Menu item ids. Region entries get a contiguous block so an id maps back to a
// country by subtraction rather than a lookup table that could drift.
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
	case id >= idRegionBase && id < idRegionBase+int32(len(regions)):
		a.doConnect(regions[id-idRegionBase].code)
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
	connecting := a.st.State == stateConnecting
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

	// Region submenu, with the current one checked.
	sub := layoutItem{ID: idRegion, Properties: submenuProps("Region"), Children: []dbus.Variant{}}
	for i, r := range regions {
		props := enabled(r.name, !busy)
		props["toggle-type"] = dbus.MakeVariant("radio")
		state := int32(0)
		if r.code == a.country {
			state = 1
		}
		props["toggle-state"] = dbus.MakeVariant(state)
		sub.Children = append(sub.Children, dbus.MakeVariant(layoutItem{
			ID:         idRegionBase + int32(i),
			Properties: props,
		}))
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
		}
		if a.st.ExitIP != "" {
			line += " (" + a.st.ExitIP + ")"
		}
		return line
	case a.st.State == stateConnecting:
		return "Connecting…"
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
