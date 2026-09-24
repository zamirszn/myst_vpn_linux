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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mystd:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		socket     = flag.String("socket", ipc.DefaultSocketPath, "unix socket to listen on")
		configPath = flag.String("config", daemon.DefaultConfigPath, "WireGuard config path")
		tokenPath  = flag.String("token", daemon.DefaultTokenPath, "credential store path")
		baseURL    = flag.String("api", "", "override the API base URL")
		noWakeLock = flag.Bool("no-wakelock", false, "allow the machine to suspend while connected")
		noIPv6     = flag.Bool("no-ipv6", false, "do not route IPv6 through the tunnel (leaks on IPv6 hosts)")
		verbose    = flag.Bool("v", false, "verbose logging")
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
	}, daemon.Deps{Logger: log})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &ipc.Server{Path: *socket, Handler: d}
	if err := srv.Listen(); err != nil {
		return err
	}
	log.Info("listening", "socket", *socket)

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
