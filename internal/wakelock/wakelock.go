// Package wakelock keeps the machine from suspending while the tunnel is up.
//
// Why this exists: suspend silently kills a WireGuard tunnel. The interface
// survives the sleep, so nothing looks wrong on resume, but the handshake is
// stale and traffic quietly stops flowing — or worse, leaks in the window before
// it re-establishes. Taking a logind inhibitor for the lifetime of the
// connection removes the problem at the source.
//
// Note this is about *suspend*, not logout. The daemon is a system service, so
// it is unaffected by user logout; no lingering is required.
package wakelock

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// UnsupportedError reports that the platform cannot take a wake lock.
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string { return "wake lock unavailable: " + e.Reason }

// Lock holds an inhibitor for as long as it is alive.
type Lock struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	active bool
}

// Acquire blocks suspend and idle until Release is called.
//
// Implemented by holding a `systemd-inhibit` child process open: the inhibitor
// lasts exactly as long as the process does, so a daemon crash releases it
// automatically rather than leaving the machine unable to sleep forever. That
// property is worth more than the marginal tidiness of a D-Bus call, and it
// keeps this package dependency-free.
func Acquire(ctx context.Context, why string) (*Lock, error) {
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		return nil, &UnsupportedError{Reason: "systemd-inhibit not found (no systemd?)"}
	}

	cmd := exec.Command("systemd-inhibit",
		"--what=sleep:idle",
		"--why="+why,
		"--mode=block",
		// systemd-inhibit holds the lock for the lifetime of this command, so
		// the command itself should do nothing but wait.
		"sleep", "infinity")

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start systemd-inhibit: %w", err)
	}

	l := &Lock{cmd: cmd, active: true}

	// Reap the child if it exits on its own so it does not linger as a zombie.
	go func() {
		cmd.Wait()
		l.mu.Lock()
		l.active = false
		l.mu.Unlock()
	}()

	return l, nil
}

// Active reports whether the lock is currently held.
func (l *Lock) Active() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// Release drops the inhibitor. Safe to call on a nil Lock and safe to call twice,
// so callers can defer it unconditionally.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	cmd, active := l.cmd, l.active
	l.active = false
	l.mu.Unlock()

	if !active || cmd == nil || cmd.Process == nil {
		return
	}
	// Killing the child releases the inhibitor.
	cmd.Process.Kill()
}
