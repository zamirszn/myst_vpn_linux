package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/auth"
	"github.com/zamirszn/mysterium-linux-client/internal/ipc"
)

// This file is the daemon's half of the magic-link login.
//
// The flow runs here rather than in each front end for three reasons. The PKCE
// verifier is what makes an emailed link redeemable, so it belongs in the one
// process that already guards the credentials, not in a window or a terminal
// that anything running as the user could inspect. The three front ends (CLI,
// window, and whatever comes next) then cannot disagree about what pasting a
// link means. And the front ends stay what the rest of the project says they
// are: they hold no credential, not even for the moment between redeeming a
// link and handing the result over.

// MagicLinker is the part of the auth server the login flow talks to. It is
// satisfied by *auth.Exchanger; tests substitute a fake.
type MagicLinker interface {
	RequestMagicLink(ctx context.Context, email, challenge string) error
	ExchangeCode(ctx context.Context, verifier, code string) (*auth.Token, error)
}

// loginLinkTTL is how long the daemon keeps a verifier waiting for its link.
//
// The email says the link is good for a few minutes; this is deliberately
// longer, so that the daemon is never the reason a link that the server would
// still accept gets refused. The server's own expiry is the real limit, and its
// rejection is reported in plain words. The point of having one at all is that
// a verifier left in memory indefinitely is a secret with no reason to exist.
const loginLinkTTL = 15 * time.Minute

// pendingLogin is a login that has asked for its link and is waiting for it.
type pendingLogin struct {
	email    string
	verifier string
	started  time.Time
}

// LoginStart asks the API to email a login link to the address.
func (d *Daemon) LoginStart(ctx context.Context, email string) (*ipc.Status, error) {
	email = strings.TrimSpace(email)
	if !plausibleEmail(email) {
		return nil, errors.New("that does not look like an email address")
	}

	verifier, challenge, err := auth.NewPKCE()
	if err != nil {
		return nil, err
	}
	if err := d.magic.RequestMagicLink(ctx, email, challenge); err != nil {
		// The earlier request, if any, is left alone: its link may still be on
		// its way to an inbox, and a failed second attempt should not cancel it.
		return nil, err
	}

	// Replaces any earlier request, so only the newest link is redeemable. The
	// older one is not wasted by this — it is simply no longer ours to finish,
	// which is the right outcome for someone who asked twice.
	d.loginMu.Lock()
	d.pending = &pendingLogin{email: email, verifier: verifier, started: time.Now()}
	d.loginMu.Unlock()

	// The address is not logged: the journal is world-readable on many systems
	// and an email is personal data that nothing here needs to keep.
	d.log.Info("login link requested")
	return d.snapshot(), nil
}

// LoginFinish redeems the pasted link, or the code in it, and stores the
// credentials it yields. As with Login, a tunnel that is up is closed first.
func (d *Daemon) LoginFinish(ctx context.Context, pasted string) (*ipc.Status, error) {
	d.loginMu.Lock()
	p := d.pending
	d.loginMu.Unlock()

	if p == nil || time.Since(p.started) > loginLinkTTL {
		return nil, errors.New("no login is waiting for a link — ask for a new one")
	}

	code := auth.ParseCode(pasted)
	if code == "" {
		// Not an exchange attempt, so the pending login is kept: someone who
		// pasted the wrong thing should be able to paste again without waiting
		// for another email.
		return nil, errors.New("that did not contain a login code — paste the whole link from the email, or the code on its own")
	}

	tok, err := d.magic.ExchangeCode(ctx, p.verifier, code)
	if err != nil {
		return nil, err
	}

	// A code is single-use. From here on this pending login cannot be finished
	// again whatever happens next, so it goes now rather than lingering as a
	// verifier that can never be redeemed.
	d.loginMu.Lock()
	if d.pending == p {
		d.pending = nil
	}
	d.loginMu.Unlock()

	// The token response does not say whose it is, but we do.
	tok.Email = p.email

	raw, err := json.Marshal(tok)
	if err != nil {
		return nil, fmt.Errorf("encode credentials: %w", err)
	}
	return d.Login(ctx, raw)
}

// Account reports whether credentials are stored and whom they were issued for.
//
// It reads the file each time instead of caching: it costs one small read per
// status poll, and it cannot go stale if something else changes the file.
func (d *Daemon) Account() (bool, string) {
	tok, err := d.store.Load()
	if err != nil || tok.AccessToken == "" {
		return false, ""
	}
	return true, tok.Email
}

// plausibleEmail rejects the inputs that are certainly not an address, so a
// typo costs nothing instead of a round trip. It is deliberately not a
// validator: the API is the authority on what it will mail, and being stricter
// than it would lock out real addresses.
func plausibleEmail(s string) bool {
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	at := strings.LastIndex(s, "@")
	if at < 1 || at == len(s)-1 {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}
