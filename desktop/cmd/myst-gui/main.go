// Command myst is the Mysterium VPN desktop application.
//
// The release binary is both the GTK front end and the privileged daemon. By
// default it opens the GUI. The same executable is re-launched as `--daemon`
// through pkexec when the control daemon is not already running.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gio "github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/zamirszn/mysterium-linux-client/desktop/internal/daemonclient"
	"github.com/zamirszn/mysterium-linux-client/internal/daemon"
)

var version = "dev"

func main() {
	log.SetFlags(0)
	log.SetPrefix("myst: ")

	// The daemon is the same release executable. This path is intentionally
	// hidden from normal GUI users; pkexec invokes it when the GUI needs the
	// privileged backend.
	if len(os.Args) > 1 && os.Args[1] == "--daemon" {
		if err := daemon.RunServer(os.Args[2:]); err != nil {
			log.Printf("daemon: %v", err)
			os.Exit(1)
		}
		return
	}

	fs := flag.NewFlagSet("myst", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", daemonclient.DefaultSocketPath, "path to the daemon's control socket")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	if err := ensureDaemon(*socket); err != nil {
		// Keep the GUI usable so the user can see the actual failure instead of
		// getting a silent process exit. Requests will show the same error in the
		// normal UI if the daemon could not be started.
		log.Printf("daemon startup: %v", err)
	}

	app := gtk.NewApplication("io.github.zamirszn.mysterium", gio.ApplicationFlagsNone)

	// The app id is registered on the session bus, so launching a second copy
	// activates the first instead of starting a rival window.
	var win *window
	app.ConnectActivate(func() {
		if win != nil {
			win.present()
			return
		}
		win = newWindow(app, daemonclient.Client{Path: *socket})
		win.present()
	})

	// Only argv[0] is passed on: GApplication must not see our private flags.
	os.Exit(app.Run(os.Args[:1]))
}

// ensureDaemon makes the GUI self-contained. If a system service is already
// running this is a cheap status request. Otherwise the same executable is
// started in privileged mode through polkit's pkexec helper.
func ensureDaemon(socket string) error {
	client := daemonclient.Client{Path: socket}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	_, err := client.Status(ctx)
	cancel()
	if err == nil {
		return nil
	}

	if _, lookErr := exec.LookPath("pkexec"); lookErr != nil {
		return fmt.Errorf("daemon is not running and pkexec is unavailable: %w", lookErr)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find myst executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve myst executable: %w", err)
	}

	group, err := currentPrimaryGroup()
	if err != nil {
		return fmt.Errorf("find desktop socket group: %w", err)
	}

	args := []string{exe, "--daemon", "-socket", socket, "-socket-group", group}
	cmd := exec.Command("pkexec", args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start privileged daemon: %w", err)
	}

	// pkexec stays attached to the daemon process, so deliberately do not Wait
	// here. Poll until the socket is ready, or until authentication/startup has
	// clearly failed.
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		_, statusErr := client.Status(ctx)
		cancel()
		if statusErr == nil {
			return nil
		}
		lastErr = statusErr
		time.Sleep(250 * time.Millisecond)
	}

	if lastErr == nil {
		lastErr = err
	}
	return fmt.Errorf("daemon did not become ready: %w", lastErr)
}

func currentPrimaryGroup() (string, error) {
	me, err := user.Current()
	if err != nil {
		return "", err
	}
	gid := strings.TrimSpace(me.Gid)
	if gid == "" {
		return "", errors.New("current user has no primary group")
	}
	if _, err := strconv.ParseUint(gid, 10, 32); err != nil {
		return "", fmt.Errorf("invalid primary gid %q: %w", gid, err)
	}
	g, err := user.LookupGroupId(gid)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(g.Name)
	if name == "" {
		return "", errors.New("primary group has no name")
	}
	return name, nil
}
