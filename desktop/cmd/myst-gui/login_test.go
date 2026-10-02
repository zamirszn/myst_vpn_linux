package main

import (
	"errors"
	"strings"
	"testing"
)

// TestAnOutOfDateDaemonIsNamedAsTheCause covers the failure a person cannot fix
// by trying again. A daemon built before logging in existed answers "unknown
// command", and showing that verbatim reads as the window's own bug.
func TestAnOutOfDateDaemonIsNamedAsTheCause(t *testing.T) {
	got := loginErrorText(errors.New(`unknown command "login-start"`))
	if !strings.Contains(got, "older") {
		t.Errorf("got %q, want it to say the daemon is out of date", got)
	}
}

// TestOtherLoginErrorsPassThrough: the daemon words its own failures for a
// person — "no account for that address" and the like — so they are not
// replaced with something vaguer.
func TestOtherLoginErrorsPassThrough(t *testing.T) {
	msg := "the API has no account for someone@example.com — check the address"
	if got := loginErrorText(errors.New(msg)); got != msg {
		t.Errorf("got %q, want the daemon's own message %q", got, msg)
	}
}
