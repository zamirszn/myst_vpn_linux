package daemon

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
	"github.com/zamirszn/mysterium-linux-client/internal/killswitch"
)

// RunServer starts the privileged control daemon. It is shared by the legacy
// mystd entry point and the single-binary `myst --daemon` release.
func RunServer(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		socket       = fs.String("socket", ipc.DefaultSocketPath, "unix socket to listen on")
		socketGroup  = fs.String("socket-group", "", "group allowed to drive the daemon (default: root only)")
		configPath   = fs.String("config", DefaultConfigPath, "WireGuard config path")
		tokenPath    = fs.String("token", DefaultTokenPath, "credential store path")
		baseURL      = fs.String("api", "", "override the API base URL")
		noWakeLock   = fs.Bool("no-wakelock", false, "allow the machine to suspend while connected")
		noIPv6       = fs.Bool("no-ipv6", false, "do not route IPv6 through the tunnel (leaks on IPv6 hosts)")
		noKillSwitch = fs.Bool("no-killswitch", false, "do not block traffic outside the tunnel when it drops")
		noMonitor    = fs.Bool("no-monitor", false, "do not watch a live tunnel for drops and reconnect it")
		verbose      = fs.Bool("v", false, "verbose logging")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if os.Geteuid() != 0 {
		log.Warn("not running as root — bringing a tunnel up will fail")
	}

	d := New(Config{
		BaseURL:    *baseURL,
		TokenPath:  *tokenPath,
		ConfigPath: *configPath,
		IPv6:       !*noIPv6,
		WakeLock:   !*noWakeLock,
		KillSwitch: !*noKillSwitch,
		Monitor:    !*noMonitor,
	}, Deps{Logger: log})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &ipc.Server{Path: *socket, Handler: d, Group: *socketGroup}
	if err := srv.Listen(); err != nil {
		return err
	}
	defer srv.Close()
	log.Info("listening", "socket", *socket)
	if *socketGroup == "" {
		log.Info("control socket is root-only; pass -socket-group to let a desktop user drive the daemon")
	} else {
		log.Info("control socket group", "group", *socketGroup)
	}

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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Close(shutdownCtx); err != nil {
		log.Error("teardown during shutdown failed", "err", err)
	}
	return nil
}

// DaemonUsage is kept here so the single-binary command can expose the same
// flags as the old mystd executable without duplicating their definitions.
func DaemonUsage() string {
	return fmt.Sprintf("myst --daemon [options]\n\nUse --help after --daemon for daemon options.")
}
