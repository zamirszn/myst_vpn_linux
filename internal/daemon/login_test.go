package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zamirszn/mysterium-linux-client/internal/auth"
)

// fakeMagic stands in for the auth server's two login endpoints.
type fakeMagic struct {
	mu sync.Mutex

	requests    []magicRequest
	requestErr  error
	exchanges   []magicExchange
	exchangeErr error
}

type magicRequest struct{ email, challenge string }
type magicExchange struct{ verifier, code string }

func (f *fakeMagic) RequestMagicLink(_ context.Context, email, challenge string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.requestErr != nil {
		return f.requestErr
	}
	f.requests = append(f.requests, magicRequest{email, challenge})
	return nil
}

func (f *fakeMagic) ExchangeCode(_ context.Context, verifier, code string) (*auth.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges = append(f.exchanges, magicExchange{verifier, code})
	if f.exchangeErr != nil {
		return nil, f.exchangeErr
	}
	// A fresh value each time: the daemon fills in the email on what it gets.
	return &auth.Token{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 3600}, nil
}

func (f *fakeMagic) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeMagic) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.exchanges)
}

func newLoginDaemon(t *testing.T) (*testEnv, *fakeMagic) {
	t.Helper()
	magic := &fakeMagic{}
	env := newTestDaemon(t, func(_ *Config, _ *testEnv, deps *Deps) {
		deps.Magic = magic
	})
	return env, magic
}

// TestLoginStoresTheTokenAndWhoItIsFor walks the whole flow and checks the
// three things that matter: the link is requested for the address typed, the
// verifier held back is the one that matches the challenge that was sent, and
// what lands on disk is the new token labelled with the address.
func TestLoginStoresTheTokenAndWhoItIsFor(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	if _, err := env.d.LoginStart(ctx, "  someone@example.com "); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if magic.requestCount() != 1 {
		t.Fatalf("%d links requested, want 1", magic.requestCount())
	}
	if got := magic.requests[0].email; got != "someone@example.com" {
		t.Errorf("link requested for %q, want the trimmed address", got)
	}

	if _, err := env.d.LoginFinish(ctx, "https://app.example/cb?code=abcd1234efgh"); err != nil {
		t.Fatalf("LoginFinish: %v", err)
	}

	ex := magic.exchanges[0]
	if ex.code != "abcd1234efgh" {
		t.Errorf("exchanged code %q, want the one in the pasted link", ex.code)
	}
	// The pair has to match, or the server rejects every login and the error
	// blames the link.
	if auth.ChallengeFor(ex.verifier) != magic.requests[0].challenge {
		t.Error("the verifier redeemed is not the pair of the challenge that was sent")
	}

	tok, err := env.d.store.Load()
	if err != nil {
		t.Fatalf("load stored token: %v", err)
	}
	if tok.AccessToken != "fresh-access" {
		t.Errorf("stored access token %q, want the one from the exchange", tok.AccessToken)
	}
	if tok.Email != "someone@example.com" {
		t.Errorf("stored email %q, want someone@example.com", tok.Email)
	}
}

// TestFinishWithoutStartIsRefused covers the daemon having restarted between
// the two halves, which loses the verifier. The person has to be told to ask
// again, not to retry a paste that can never work.
func TestFinishWithoutStartIsRefused(t *testing.T) {
	env, magic := newLoginDaemon(t)

	_, err := env.d.LoginFinish(context.Background(), "https://app.example/cb?code=abcd1234efgh")
	if err == nil {
		t.Fatal("a login nobody started was accepted")
	}
	if magic.exchangeCount() != 0 {
		t.Error("a code was sent to the server with no verifier to go with it")
	}
}

// TestPastingTheWrongThingKeepsTheLoginWaiting is the forgiving half. Someone
// who pastes the wrong line should be able to paste the right one without
// another email — and without having spent a single-use code on nothing.
func TestPastingTheWrongThingKeepsTheLoginWaiting(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}

	if _, err := env.d.LoginFinish(ctx, "https://app.example/nothing-here"); err == nil {
		t.Fatal("a link with no code in it was accepted")
	}
	if magic.exchangeCount() != 0 {
		t.Fatal("a paste with no code reached the server")
	}

	if _, err := env.d.LoginFinish(ctx, "https://app.example/cb?code=abcd1234efgh"); err != nil {
		t.Errorf("the second, correct paste was refused: %v", err)
	}
}

// TestACodeCannotBeRedeemedTwice: once a link has been used, the login is over.
func TestACodeCannotBeRedeemedTwice(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if _, err := env.d.LoginFinish(ctx, "abcd1234efgh"); err != nil {
		t.Fatalf("LoginFinish: %v", err)
	}
	if _, err := env.d.LoginFinish(ctx, "abcd1234efgh"); err == nil {
		t.Error("the same login was finished twice")
	}
	if magic.exchangeCount() != 1 {
		t.Errorf("%d exchanges, want 1", magic.exchangeCount())
	}
}

// TestAnExpiredWaitIsRefused keeps a verifier from living forever.
func TestAnExpiredWaitIsRefused(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	env.d.loginMu.Lock()
	env.d.pending.started = time.Now().Add(-2 * loginLinkTTL)
	env.d.loginMu.Unlock()

	if _, err := env.d.LoginFinish(ctx, "abcd1234efgh"); err == nil {
		t.Error("a login that waited past its limit was accepted")
	}
	if magic.exchangeCount() != 0 {
		t.Error("an expired login still reached the server")
	}
}

// TestNotAnEmailCostsNoRequest saves the round trip, and the API's own error.
func TestNotAnEmailCostsNoRequest(t *testing.T) {
	env, magic := newLoginDaemon(t)

	for _, bad := range []string{"", "   ", "plainaddress", "@example.com", "me@", "me@localhost", "a b@example.com"} {
		if _, err := env.d.LoginStart(context.Background(), bad); err == nil {
			t.Errorf("LoginStart(%q) succeeded", bad)
		}
	}
	if magic.requestCount() != 0 {
		t.Errorf("%d links were requested for addresses that cannot be one", magic.requestCount())
	}
}

// TestAFailedRequestDoesNotCancelTheEarlierOne: the first link may already be
// in the inbox, and a hiccup on the second ask must not make it unredeemable.
func TestAFailedRequestDoesNotCancelTheEarlierOne(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}

	magic.mu.Lock()
	magic.requestErr = errors.New("the API is down")
	magic.mu.Unlock()
	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err == nil {
		t.Fatal("a failed request was reported as success")
	}

	if _, err := env.d.LoginFinish(ctx, "abcd1234efgh"); err != nil {
		t.Errorf("the earlier link no longer works after a failed resend: %v", err)
	}
}

// TestANewRequestSupersedesTheOldVerifier: only the newest link is the one the
// daemon is waiting for.
func TestANewRequestSupersedesTheOldVerifier(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("first LoginStart: %v", err)
	}
	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("second LoginStart: %v", err)
	}
	if _, err := env.d.LoginFinish(ctx, "abcd1234efgh"); err != nil {
		t.Fatalf("LoginFinish: %v", err)
	}

	if got := magic.exchanges[0].verifier; auth.ChallengeFor(got) != magic.requests[1].challenge {
		t.Error("the code was redeemed with the first request's verifier, not the latest")
	}
}

// TestAccountFollowsLoginAndLogout is what every front end flips its screen on.
func TestAccountFollowsLoginAndLogout(t *testing.T) {
	env, _ := newLoginDaemon(t)
	ctx := context.Background()

	// The harness seeds a token with no address, like one imported from a file.
	if ok, email := env.d.Account(); !ok || email != "" {
		t.Errorf("seeded account = %v %q, want signed in with no address", ok, email)
	}

	if _, err := env.d.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if ok, _ := env.d.Account(); ok {
		t.Error("still reported as signed in after logout")
	}

	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err != nil {
		t.Fatalf("LoginStart: %v", err)
	}
	if _, err := env.d.LoginFinish(ctx, "abcd1234efgh"); err != nil {
		t.Fatalf("LoginFinish: %v", err)
	}
	if ok, email := env.d.Account(); !ok || email != "someone@example.com" {
		t.Errorf("account after login = %v %q, want signed in as someone@example.com", ok, email)
	}
}

// TestAnEmptyTokenFileIsNotAnAccount: a file that exists but holds nothing
// usable must show the login screen, not a main screen that cannot connect.
func TestAnEmptyTokenFileIsNotAnAccount(t *testing.T) {
	env, _ := newLoginDaemon(t)

	if err := env.d.store.Save(&auth.Token{}); err != nil {
		t.Fatalf("overwrite token: %v", err)
	}
	if ok, _ := env.d.Account(); ok {
		t.Error("a token with no access token was reported as an account")
	}
}

// TestLoginErrorsCarryTheServersWords: the server's message is what the person
// needs to read, so it must not be swallowed on the way out.
func TestLoginErrorsCarryTheServersWords(t *testing.T) {
	env, magic := newLoginDaemon(t)
	ctx := context.Background()

	magic.requestErr = errors.New("the API has no account for someone@example.com")
	if _, err := env.d.LoginStart(ctx, "someone@example.com"); err == nil || !strings.Contains(err.Error(), "no account") {
		t.Errorf("LoginStart err = %v, want the API's own message", err)
	}
}

// TestLogoutDeletesCredentialsWhenServerCleanupFails makes local sign-out
// deterministic. A stale/expired bearer can make the server-side disconnect
// fail, but that must not leave the user permanently signed in on this machine.
func TestLogoutDeletesCredentialsWhenServerCleanupFails(t *testing.T) {
	env, _ := newLoginDaemon(t)
	ctx := context.Background()

	// The test daemon starts with a seeded token. Force the server-side cleanup
	// path to fail by using a token source that cannot refresh and an API client
	// that returns an error; local tunnel state is already disconnected.
	if err := env.d.store.Save(&auth.Token{AccessToken: "expired", RefreshToken: ""}); err != nil {
		t.Fatal(err)
	}

	if _, err := env.d.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if ok, _ := env.d.Account(); ok {
		t.Fatal("credentials still exist after local logout")
	}
}
