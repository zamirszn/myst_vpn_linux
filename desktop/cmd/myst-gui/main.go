// Command myst-gui is the Mysterium VPN window.
//
// Like the tray applet it is a pure front end: it holds no credentials, opens
// no tunnel, and has no privileges. Every action is a request over the daemon's
// control socket, which is the only component that runs as root.
//
// It is the one binary in this project that uses cgo, because GTK4 has no
// pure-Go binding. Everything else builds with the standard library alone.
package main

import (
	"flag"
	"log"
	"os"

	gio "github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/zamirszn/mysterium-linux-client/desktop/internal/daemonclient"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("myst-gui: ")

	socket := flag.String("socket", daemonclient.DefaultSocketPath, "path to the daemon's control socket")
	flag.Parse()

	app := gtk.NewApplication("io.github.zamirszn.mysterium", gio.ApplicationFlagsNone)

	// The app id is registered on the session bus, so launching a second copy
	// activates the first instead of starting a rival. That matters here: two
	// windows would show the same daemon state and fight over the same button.
	var win *window
	app.ConnectActivate(func() {
		if win != nil {
			win.present()
			return
		}
		win = newWindow(app, daemonclient.Client{Path: *socket})
		win.present()
	})

	// Only argv[0] is passed on: the flag above is ours, not GTK's, and
	// GApplication treats an option it does not recognise as a fatal error.
	os.Exit(app.Run(os.Args[:1]))
}
