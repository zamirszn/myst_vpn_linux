package wg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// InterfaceName is the tunnel interface this client manages.
const InterfaceName = "myst0"

// ToolMissingError reports that a required external binary is not installed,
// so callers can tell the user which package to install rather than showing
// a bare exec error.
type ToolMissingError struct{ Tool string }

func (e *ToolMissingError) Error() string {
	return fmt.Sprintf("%s not found — install it with: apt install wireguard-tools", e.Tool)
}

// Up brings the interface up via wg-quick.
func Up(ctx context.Context, configPath string) error {
	return run(ctx, "wg-quick", "up", configPath)
}

// Down tears the interface down and restores routes and DNS.
func Down(ctx context.Context) error {
	return run(ctx, "wg-quick", "down", InterfaceName)
}

// IsUp reports whether the interface currently exists.
func IsUp(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "ip", "link", "show", InterfaceName)
	return cmd.Run() == nil
}

// Handshake returns the latest handshake time for the interface, or the zero
// time if none has occurred. A tunnel that is "up" but never handshakes is the
// failure mode worth surfacing early.
func Handshake(ctx context.Context) (time.Time, error) {
	cmd := exec.CommandContext(ctx, "wg", "show", InterfaceName, "latest-handshakes")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("wg show: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return time.Time{}, nil
	}
	var epoch int64
	if _, err := fmt.Sscanf(fields[1], "%d", &epoch); err != nil {
		return time.Time{}, fmt.Errorf("parse handshake %q: %w", fields[1], err)
	}
	if epoch == 0 {
		return time.Time{}, nil
	}
	return time.Unix(epoch, 0), nil
}

// ExitIP reports the public IP as seen by the outside world, which is how we
// confirm traffic is actually egressing through the tunnel.
func ExitIP(ctx context.Context) (string, error) {
	return lookupIP(ctx, "https://api.ipify.org")
}

func lookupIP(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exit IP lookup: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", fmt.Errorf("read exit IP: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}

func run(ctx context.Context, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return &ToolMissingError{Tool: name}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}
