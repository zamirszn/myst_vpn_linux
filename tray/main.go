// Command myst-tray puts a Mysterium VPN icon in the desktop's system tray.
//
// It is a pure front end: it holds no credentials, opens no tunnel, and has no
// privileges. Everything it does is a request over the daemon's control socket,
// which is the only component that runs as root.
//
// This lives in its own Go module so that the daemon and the CLI keep their
// no-external-dependencies property. Talking StatusNotifierItem and dbusmenu by
// hand needs a D-Bus library, and there is none in the standard library.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	introspectableInterface = "org.freedesktop.DBus.Introspectable"

	watcherName  = "org.kde.StatusNotifierWatcher"
	watcherPath  = dbus.ObjectPath("/StatusNotifierWatcher")
	watcherIface = "org.kde.StatusNotifierWatcher"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("myst-tray: ")

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "myst-tray:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		socket  = flag.String("socket", defaultSocketPath, "path to the daemon's control socket")
		country = flag.String("country", "", "region to connect to; changeable from the menu")
	)
	flag.Parse()

	conn, err := dbus.SessionBus()
	if err != nil {
		return fmt.Errorf("connect to the session bus: %w", err)
	}
	defer conn.Close()

	a := newApp(conn, client{path: *socket}, *country)

	// The well-known name is the tray convention, and the watcher records it so
	// the panel can tell items apart.
	name := fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid())
	reply, err := conn.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("request bus name %s: %w", name, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("bus name %s is already owned by another process", name)
	}
	defer conn.ReleaseName(name)

	// godbus publishes every method of a value under the interface it is
	// exported with, which is why the item's methods, its properties, and its
	// introspection each need their own value.
	exports := []struct {
		value any
		path  dbus.ObjectPath
		iface string
	}{
		{&sniMethods{a}, sniPath, sniInterface},
		{&sniProps{a}, sniPath, propsInterface},
		{introspectable{sniIntrospectionXML}, sniPath, introspectableInterface},

		{&menuMethods{a}, menuPath, menuInterface},
		{&menuProps{a}, menuPath, propsInterface},
		{introspectable{menuIntrospectionXML}, menuPath, introspectableInterface},
	}
	for _, e := range exports {
		if err := conn.Export(e.value, e.path, e.iface); err != nil {
			return fmt.Errorf("export %s at %s: %w", e.iface, e.path, err)
		}
	}

	if err := registerWithWatcher(conn, name); err != nil {
		return err
	}
	log.Printf("registered as %s; socket %s", name, *socket)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Poll once before entering the loop, so the icon shows the real state
	// rather than a placeholder for the first interval.
	a.refresh(ctx)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.quit:
			return nil
		case <-ticker.C:
			a.refresh(ctx)
		}
	}
}

// registerWithWatcher announces the item to the panel's tray host.
func registerWithWatcher(conn *dbus.Conn, name string) error {
	obj := conn.Object(watcherName, watcherPath)

	// Check the watcher answers at all before registering, so a missing tray
	// host produces an explanation instead of a bare D-Bus error. The same call
	// reports whether a host is listening — the watcher can be running with
	// nothing attached, in which case registering succeeds and nothing appears.
	var hosted bool
	if err := obj.StoreProperty(watcherIface+".IsStatusNotifierHostRegistered", &hosted); err != nil {
		return fmt.Errorf(
			"no StatusNotifierWatcher on the session bus: %w\n"+
				"the tray needs a host; on GNOME that means the AppIndicator extension:\n"+
				"  gnome-extensions enable ubuntu-appindicators@ubuntu.com", err)
	}

	call := obj.Call(watcherIface+".RegisterStatusNotifierItem", 0, name)
	if call.Err != nil {
		return fmt.Errorf("register with the StatusNotifierWatcher: %w", call.Err)
	}
	if !hosted {
		log.Print("warning: the watcher is running but no host is registered; " +
			"the icon will appear once the panel's appindicator support starts")
	}
	return nil
}
