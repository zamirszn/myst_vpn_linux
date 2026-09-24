// Command mystd is the privileged daemon: it owns the WireGuard tunnel and
// serves the control socket that myst and the tray applet talk to.
//
// It runs as root because bringing an interface up needs CAP_NET_ADMIN. It
// exposes exactly four commands over a unix socket and nothing else — no
// network listener, no shell.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/daemon"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
	"github.com/zamirszn/mysterium-linux-client/internal/killswitch"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mystd:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		socket       = flag.String("socket", ipc.DefaultSocketPath, "unix socket to listen on")
		socketGroup  = flag.String("socket-group", "", "group allowed to drive the daemon (default: root only)")
		configPath   = flag.String("config", daemon.DefaultConfigPath, "WireGuard config path")
		tokenPath    = flag.String("token", daemon.DefaultTokenPath, "credential store path")
		baseURL      = flag.String("api", "", "override the API base URL")
		noWakeLock   = flag.Bool("no-wakelock", false, "allow the machine to suspend while connected")
		noIPv6       = flag.Bool("no-ipv6", false, "do not route IPv6 through the tunnel (leaks on IPv6 hosts)")
		noKillSwitch = flag.Bool("no-killswitch", false, "do not block traffic outside the tunnel when it drops")
		noMonitor    = flag.Bool("no-monitor", false, "do not watch a live tunnel for drops and reconnect it")
		verbose      = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if os.Geteuid() != 0 {
		// Not fatal: worth starting so the operator sees a clear message rather
		// than a mysteriously failing connect later.
		log.Warn("not running as root — bringing a tunnel up will fail")
	}

	d := daemon.New(daemon.Config{
		BaseURL:    *baseURL,
		TokenPath:  *tokenPath,
		ConfigPath: *configPath,
		IPv6:       !*noIPv6,
		WakeLock:   !*noWakeLock,
		KillSwitch: !*noKillSwitch,
		Monitor:    !*noMonitor,
	}, daemon.Deps{Logger: log})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &ipc.Server{Path: *socket, Handler: d, Group: *socketGroup}
	if err := srv.Listen(); err != nil {
		return err
	}
	log.Info("listening", "socket", *socket)
	if *socketGroup == "" {
		// Say so plainly rather than letting the operator discover it as a
		// "permission denied" from a client that looks like it should work.
		log.Info("control socket is root-only; pass -socket-group to let a desktop user drive the daemon")
	} else {
		log.Info("control socket group", "group", *socketGroup)
	}

	// Clear anything a previous run left behind. A hard kill between arming the
	// filter and bringing the tunnel up would otherwise leave the machine with
	// no network and nothing still running to remove it.
	//
	// This runs after Listen on purpose: only one daemon can hold the socket,
	// so a second instance cannot disarm the first one's filter on its way in.
	//
	// Gated on root because nft reports "operation not permitted" rather than
	// "no such table" otherwise, which would produce a misleading warning on a
	// daemon that could not have armed anything in the first place.
	if os.Geteuid() == 0 {
		ks := killswitch.Nft{}
		if armed, err := ks.Active(context.Background()); err == nil && armed {
			log.Warn("clearing an egress filter left armed by a previous run")
		}
		if err := ks.Remove(context.Background()); err != nil {
			log.Warn("could not clear a stale egress filter", "err", err)
		}
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Tear the tunnel down on the way out. Leaving an interface up with no
	// daemon to manage it is worse than dropping the connection: nothing is
	// left to bring it down, and the wake lock would go with the process anyway.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Close(shutdownCtx); err != nil {
		log.Error("teardown during shutdown failed", "err", err)
	}
	return nil
}
