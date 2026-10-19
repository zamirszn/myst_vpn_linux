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
		win = newWindow(app, daemonclient.Client{Path: *socket}, *socket)
		win.present()
	})

	// Only argv[0] is passed on: GApplication must not see our private flags.
	os.Exit(app.Run(os.Args[:1]))
}

// ensureDaemon makes the GUI self-contained. If a system service is already
// running this is a cheap status request.
//
// When it is not, an installed package is repaired through its service unit:
// the unit's launcher re-reads which desktop user is active and gives the
// socket that user's primary group, so one pkexec prompt fixes the daemon
// without anyone logging out. A copy that was not installed as a package has no
// unit, so the same executable is started in privileged mode directly.
func ensureDaemon(socket string) error {
	client := daemonclient.Client{Path: socket}
	if err := probeDaemon(client); err == nil {
		return nil
	}

	if _, lookErr := exec.LookPath("pkexec"); lookErr != nil {
		return fmt.Errorf("the VPN service is not running and pkexec is unavailable: %w", lookErr)
	}

	// The systemd unit is the right way to bring the service back: it runs the
	// launcher wrapper, which resolves the socket group for whoever is sitting
	// at the machine rather than for whoever installed the package. Do not fall
	// through to a direct daemon when the unit exists — that would mean a second
	// password prompt after the first was cancelled, and could leave two daemons
	// racing for the same socket.
	if serviceInstalled() {
		if err := restartService(client); err != nil {
			log.Printf("restarting the VPN service: %v", err)
			return err
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the Mysterium executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve the Mysterium executable: %w", err)
	}

	group, err := currentPrimaryGroup()
	if err != nil {
		return fmt.Errorf("find the desktop socket group: %w", err)
	}

	args := []string{exe, "--daemon", "-socket", socket, "-socket-group", group}
	cmd := exec.Command("pkexec", args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the privileged VPN service: %w", err)
	}

	// pkexec stays attached to the daemon process, so deliberately do not Wait
	// here. Poll until the socket is ready, or until authentication/startup has
	// clearly failed.
	if err := waitForDaemon(client, 15*time.Second); err != nil {
		return fmt.Errorf("the VPN service did not start: %w", err)
	}
	return nil
}

// restartService restarts an installed mystd through systemd under pkexec and
// waits for it to answer. The unit may be present but stopped, or it may be
// running with the wrong socket group after a desktop user change.
func restartService(client daemonclient.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "pkexec", "systemctl", "restart", "mystd").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("restart the VPN service: %s", msg)
	}
	return waitForDaemon(client, 15*time.Second)
}

func probeDaemon(client daemonclient.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := client.Status(ctx)
	return err
}

func waitForDaemon(client daemonclient.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		_, err := client.Status(ctx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timed out")
	}
	return lastErr
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// serviceInstalled reports whether a mystd systemd unit is present, in either
// the package location or the portable one. The launcher path is checked too so
// an installed package whose unit is named differently still takes the service
// path rather than starting a second daemon directly.
func serviceInstalled() bool {
	for _, path := range []string{
		"/etc/systemd/system/mystd.service",
		"/usr/lib/systemd/system/mystd.service",
		"/lib/systemd/system/mystd.service",
		"/usr/lib/mysterium-vpn/mystd-launch",
		"/usr/local/lib/mysterium-vpn/mystd-launch",
	} {
		if fileExists(path) {
			return true
		}
	}
	return false
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
