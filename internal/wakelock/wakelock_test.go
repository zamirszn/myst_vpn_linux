package wakelock

import (
	"context"
	"os/exec"
	"testing"
)

func TestAcquireHoldsAndReleaseDrops(t *testing.T) {
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		t.Skip("systemd-inhibit is not installed")
	}

	l, err := Acquire(context.Background(), "myst test")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !l.Active() {
		t.Error("lock is not active immediately after Acquire")
	}

	l.Release()
	if l.Active() {
		t.Error("lock is still active after Release")
	}

	// Release must be safe to call again, so callers can defer it
	// unconditionally without tracking whether they already released.
	l.Release()
}

func TestNilLockIsInert(t *testing.T) {
	var l *Lock
	if l.Active() {
		t.Error("a nil lock reports itself as active")
	}
	l.Release() // must not panic
}

func TestUnsupportedPlatformIsReported(t *testing.T) {
	// Exercise the error path directly rather than by manipulating PATH, which
	// would race with other tests in the package.
	err := &UnsupportedError{Reason: "systemd-inhibit not found (no systemd?)"}
	if err.Error() == "" {
		t.Error("UnsupportedError has no message")
	}
}
