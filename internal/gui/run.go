package gui

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

	"github.com/zamirszn/mysterium-linux-client/internal/daemon"
	"github.com/zamirszn/mysterium-linux-client/internal/daemonclient"
)

// Run starts the desktop application. The daemon is intentionally handled by
// the same executable; GUI users never need to invoke a separate daemon binary.
func Run(args []string) int {
	log.SetFlags(0)
	log.SetPrefix("myst: ")

	fs := flag.NewFlagSet("myst", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", daemonclient.DefaultSocketPath, "path to the daemon's control socket")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := ensureDaemon(*socket); err != nil {
		log.Printf("daemon startup: %v", err)
	}

	app := gtk.NewApplication("io.github.zamirszn.mysterium", gio.ApplicationFlagsNone)
	var win *window
	app.ConnectActivate(func() {
		if win != nil {
			win.present()
			return
		}
		win = newWindow(app, daemonclient.Client{Path: *socket})
		win.present()
	})
	return app.Run([]string{"myst"})
}

func RunDaemon(args []string) error { return daemon.RunServer(args) }

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
	cmd := exec.Command("pkexec", exe, "--daemon", "-socket", socket, "-socket-group", group)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start privileged daemon: %w", err)
	}
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
