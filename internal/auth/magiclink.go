package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file implements the first half of getting a token: the magic link.
//
// The flow is three steps, and the middle one cannot be automated away:
//
//	1. POST /magic-link with a PKCE challenge we generated
//	2. the user clicks the link in their email — this is the step that proves
//	   they control the address, so nothing we do can substitute for it
//	3. POST /oauth/token with the code that link carries and the matching
//	   verifier from step 1
//
// Step 2 is why this is a paste rather than a callback. The email link can be
// asked to redirect to a local address with continue_to, but the API is not
// obliged to honour it — the earlier shell harness that tried this carried a
// paste-the-link fallback for exactly that reason, and a flow whose success
// depends on an undocumented parameter being respected is a flow that breaks
// without warning. Pasting the link always works, so it is the only path here.
//
// The verifier must come from the client that requested the link: a link
// minted elsewhere cannot be completed with a verifier generated here. That is
// the whole point of PKCE, and it means the code is useless to anyone who
// intercepts the email but not this process's memory.

// Login walks the interactive half of the magic-link flow.
//
// present is called once, with the message the user needs to act on, so the
// caller decides how to show it (the CLI prints it; a test captures it).
// ask is called to read back what they pasted.
//
// It is deliberately two callbacks rather than reading stdin directly: the
// flow is then testable without a terminal, and adding a window later means
// passing a different pair rather than rewriting this.
func (e *Exchanger) Login(ctx context.Context, email string, present func(string), ask func() (string, error)) (*Token, error) {
	if strings.TrimSpace(email) == "" {
		return nil, fmt.Errorf("an email address is required to send a magic link")
	}

	verifier, challenge, err := NewPKCE()
	if err != nil {
		return nil, err
	}

	if err := e.RequestMagicLink(ctx, email, challenge); err != nil {
		return nil, err
	}

	present(fmt.Sprintf(
		"A login link is on its way to %s.\n\n"+
			"Open it, then paste the link you were sent (or just the code from it) back here.\n"+
			"The link is only good for a few minutes.",
		email))

	answer, err := ask()
	if err != nil {
		return nil, err
	}

	code := ParseCode(answer)
	if code == "" {
		return nil, fmt.Errorf("that did not contain a login code — paste the whole link from the email, or the code on its own")
	}

	return e.ExchangeCode(ctx, verifier, code)
}

// RequestMagicLink asks the API to email a login link carrying our challenge.
func (e *Exchanger) RequestMagicLink(ctx context.Context, email, challenge string) error {
	body, err := json.Marshal(map[string]string{
		"email":                 strings.TrimSpace(email),
		"client_id":             ClientID,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	})
	if err != nil {
		return fmt.Errorf("encode magic-link request: %w", err)
	}

	raw, status, err := e.post(ctx, "/magic-link", body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		// The one failure worth its own wording. Asking for a link for an address
		// with no account is the most likely reason to be here, and "HTTP 404"
		// would send someone to check their network instead of their spelling.
		if status == http.StatusNotFound {
			return fmt.Errorf("the API has no account for %s — check the address, or sign up at mysteriumvpn.com", email)
		}
		return fmt.Errorf("request magic link: HTTP %d: %s", status, truncate(string(raw), 300))
	}
	return nil
}

// ExchangeCode trades the emailed code for a token set.
func (e *Exchanger) ExchangeCode(ctx context.Context, verifier, code string) (*Token, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     ClientID,
		"code_verifier": verifier,
		"code":          code,
	})
	if err != nil {
		return nil, fmt.Errorf("encode token request: %w", err)
	}

	raw, status, err := e.post(ctx, "/oauth/token", body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			// Overwhelmingly this is an expired or already-used link. The API's
			// own wording for it is not something a person can act on.
			return nil, fmt.Errorf("that login link was rejected — it may have expired or been used already; run the command again for a new one")
		}
		return nil, fmt.Errorf("exchange login code: HTTP %d: %s", status, truncate(string(raw), 300))
	}

	var out Token
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("the API returned no access_token")
	}
	// The server reports a relative lifetime; the rest of the client measures
	// expiry from when we received it.
	out.ObtainedAt = time.Now()
	return &out, nil
}

func (e *Exchanger) post(ctx context.Context, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s request: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read %s response: %w", path, err)
	}
	return raw, resp.StatusCode, nil
}

// NewPKCE returns a verifier and its S256 challenge, per RFC 7636.
//
// The verifier is 32 random bytes in base64url — 43 characters, the shortest
// the spec allows, which is also the most that fits comfortably in a link.
func NewPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	return verifier, ChallengeFor(verifier), nil
}

// ChallengeFor derives the S256 challenge from a verifier.
func ChallengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ParseCode pulls the login code out of whatever the user pasted.
//
// People paste the whole link, and links arrive in several shapes: a query
// parameter, a fragment, or a bare code copied out of one. Being liberal here
// is the difference between a flow that works and one that makes someone
// hand-extract a URL parameter.
func ParseCode(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return ""
	}

	// A whole link, or anything with a query string in it.
	u, err := url.Parse(s)
	if err == nil {
		if code := firstNonEmpty(
			u.Query().Get("code"),
			u.Query().Get("authorization_code"),
			u.Query().Get("activation_code"),
		); code != "" {
			return code
		}
		// Some flows put it in the fragment instead.
		if u.Fragment != "" {
			if frag, err := url.ParseQuery(u.Fragment); err == nil {
				if code := firstNonEmpty(frag.Get("code"), frag.Get("authorization_code")); code != "" {
					return code
				}
			}
		}
	}

	// A link with no code in it is a dead end, and saying so is the whole point
	// of this check. Falling through would hand the entire URL back as the code:
	// it has no whitespace and is comfortably long enough to look plausible, so
	// the API would reject it and the user would be told their link had expired
	// — sending them to request another link that fails the same way, when the
	// real problem is that they pasted the wrong one.
	if err == nil && u.Scheme != "" && u.Host != "" {
		return ""
	}

	// Not a URL. If what is left looks like a code rather than prose, take it at
	// face value — this is the "just the code" case, and also a link shape we
	// have not seen.
	if isPlausibleCode(s) {
		return s
	}
	return ""
}

// isPlausibleCode rejects the things a person might paste by mistake — a
// sentence, an email address, a bare URL with nothing in it — without being so
// strict that it rejects a code whose alphabet we have not seen.
func isPlausibleCode(s string) bool {
	if len(s) < 8 || len(s) > 4096 {
		return false
	}
	// Whitespace means it is prose, not a token. A real code is one unbroken run.
	return !strings.ContainsAny(s, " \t\n\r")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
