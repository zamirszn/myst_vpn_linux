package main

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// toolTip is the (sa(iiay)ss) structure the StatusNotifierItem spec uses.
type toolTip struct {
	IconName   string
	IconPixmap []pixmap
	Title      string
	Text       string
}

// sniMethods implements org.kde.StatusNotifierItem.
//
// godbus exports every method of a value under the interface it is registered
// with, so the properties and introspection entry points have to live on
// separate types. Registering one struct that carried all three sets would
// publish Get and Introspect as StatusNotifierItem methods too.
type sniMethods struct{ a *app }

func (s *sniMethods) Activate(x, y int32) *dbus.Error {
	// Left click. Toggling is the convention for a VPN applet and saves a trip
	// through the menu for the one action people actually take.
	s.a.toggle()
	return nil
}

func (s *sniMethods) SecondaryActivate(x, y int32) *dbus.Error { return nil }

func (s *sniMethods) ContextMenu(x, y int32) *dbus.Error {
	// The host owns menu display; it reads our layout from dbusmenu when it
	// decides to show one. Nothing to do here.
	return nil
}

func (s *sniMethods) Scroll(delta int32, orientation string) *dbus.Error { return nil }

// sniProps implements org.freedesktop.DBus.Properties for the item.
type sniProps struct{ a *app }

func (p *sniProps) Get(iface, name string) (dbus.Variant, *dbus.Error) {
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

func (p *sniProps) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	if iface != sniInterface {
		return nil, unknownInterface(iface)
	}

	a := p.a
	a.mu.Lock()
	defer a.mu.Unlock()

	return map[string]dbus.Variant{
		"Category": dbus.MakeVariant("ApplicationStatus"),
		"Id":       dbus.MakeVariant("myst-tray"),
		"Title":    dbus.MakeVariant("Mysterium VPN"),
		"Status":   dbus.MakeVariant(a.sniStatus),

		// IconName is left empty on purpose. A host that can resolve a themed
		// name will prefer it over IconPixmap, and a themed icon cannot show
		// which state the tunnel is in — the whole point of drawing our own.
		"IconName":   dbus.MakeVariant(""),
		"IconPixmap": dbus.MakeVariant(a.icon),

		"ToolTip": dbus.MakeVariant(toolTip{
			Title:      "Mysterium VPN",
			Text:       a.tooltip,
			IconPixmap: a.icon,
		}),

		"ItemIsMenu":    dbus.MakeVariant(false),
		"Menu":          dbus.MakeVariant(menuPath),
		"WindowId":      dbus.MakeVariant(int32(0)),
		"IconThemePath": dbus.MakeVariant([]string{}),

		// Nothing here raises a separate attention icon, but hosts query these
		// unconditionally and are happier with an empty answer than an error.
		"AttentionIconName":   dbus.MakeVariant(""),
		"AttentionIconPixmap": dbus.MakeVariant([]pixmap{}),
		"AttentionMovieName":  dbus.MakeVariant(""),
		"OverlayIconName":     dbus.MakeVariant(""),
		"OverlayIconPixmap":   dbus.MakeVariant([]pixmap{}),
	}, nil
}

func (p *sniProps) Set(iface, name string, value dbus.Variant) *dbus.Error {
	return readOnly(name)
}

// introspectable serves org.freedesktop.DBus.Introspectable. godbus does not
// provide one, and a few hosts (waybar, KDE) ask before they will talk to an
// item at all.
type introspectable struct{ xml string }

func (i introspectable) Introspect() (string, *dbus.Error) { return i.xml, nil }

const sniIntrospectionXML = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
  <interface name="org.kde.StatusNotifierItem">
    <method name="Activate"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
    <method name="SecondaryActivate"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
    <method name="ContextMenu"><arg name="x" type="i" direction="in"/><arg name="y" type="i" direction="in"/></method>
    <method name="Scroll"><arg name="delta" type="i" direction="in"/><arg name="orientation" type="s" direction="in"/></method>
    <signal name="NewIcon"/>
    <signal name="NewStatus"><arg name="status" type="s"/></signal>
    <signal name="NewToolTip"/>
    <signal name="NewTitle"/>
    <property name="Category" type="s" access="read"/>
    <property name="Id" type="s" access="read"/>
    <property name="Title" type="s" access="read"/>
    <property name="Status" type="s" access="read"/>
    <property name="IconName" type="s" access="read"/>
    <property name="IconPixmap" type="a(iiay)" access="read"/>
    <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
    <property name="ItemIsMenu" type="b" access="read"/>
    <property name="Menu" type="o" access="read"/>
    <property name="WindowId" type="i" access="read"/>
    <property name="IconThemePath" type="as" access="read"/>
    <property name="AttentionIconName" type="s" access="read"/>
    <property name="AttentionIconPixmap" type="a(iiay)" access="read"/>
    <property name="AttentionMovieName" type="s" access="read"/>
    <property name="OverlayIconName" type="s" access="read"/>
    <property name="OverlayIconPixmap" type="a(iiay)" access="read"/>
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

// Well-known D-Bus errors. godbus supplies helpers for the method-not-found
// family but not these, and a host that gets the wrong error name may treat a
// typo in its own property lookup as a fatal problem.
func unknownProperty(name string) *dbus.Error {
	return &dbus.Error{
		Name: "org.freedesktop.DBus.Error.UnknownProperty",
		Body: []any{fmt.Sprintf("no such property %q", name)},
	}
}

func unknownInterface(name string) *dbus.Error {
	return &dbus.Error{
		Name: "org.freedesktop.DBus.Error.UnknownInterface",
		Body: []any{fmt.Sprintf("no such interface %q", name)},
	}
}

func readOnly(name string) *dbus.Error {
	return &dbus.Error{
		Name: "org.freedesktop.DBus.Error.PropertyReadOnly",
		Body: []any{fmt.Sprintf("property %q is read-only", name)},
	}
}
