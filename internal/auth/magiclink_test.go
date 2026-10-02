package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChallengeForMatchesTheSpec pins S256 to RFC 7636's own worked example.
//
// This is the one part of the login flow that cannot be checked by trying it:
// get the transformation subtly wrong — base64 instead of base64url, the raw
// verifier instead of its digest — and the server rejects every login with a
// message about the code being invalid, which points at the email rather than
// at the arithmetic. The spec publishes a verifier and its expected challenge,
// so there is no reason to test this against our own output.
func TestChallengeForMatchesTheSpec(t *testing.T) {
	// RFC 7636, Appendix B.
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if got := ChallengeFor(verifier); got != challenge {
		t.Errorf("ChallengeFor(spec verifier) = %q, want %q", got, challenge)
	}
}

// TestNewPKCEGeneratesAUsablePair covers what the API will insist on: the
// verifier must be in the unreserved character set and long enough to matter,
// and the challenge must be its digest. A verifier with a "+" or "/" in it
// survives JSON but can be mangled in transit by anything that treats it as a
// URL component.
func TestNewPKCEGeneratesAUsablePair(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		verifier, challenge, err := NewPKCE()
		if err != nil {
			t.Fatalf("NewPKCE: %v", err)
		}
		if len(verifier) < 43 || len(verifier) > 128 {
			t.Fatalf("verifier length %d is outside the range RFC 7636 allows", len(verifier))
		}
		for _, r := range verifier {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~", r) {
				t.Fatalf("verifier contains %q, which is not in the unreserved set", r)
			}
		}
		if want := ChallengeFor(verifier); challenge != want {
			t.Fatalf("challenge is not the digest of its own verifier")
		}
		// Two calls must not produce the same pair, or the challenge is not
		// protecting anything.
		if seen[verifier] {
			t.Fatal("NewPKCE returned the same verifier twice")
		}
		seen[verifier] = true
	}
}

func TestParseCode(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"a full HTTPS link", "https://mysteriumvpn.com/auth/callback?code=abc123def456&state=x", "abc123def456"},
		{"a link with the code first", "https://x.test/cb?code=abc123def456", "abc123def456"},
		{"a bare code", "abc123def456", "abc123def456"},
		{"a code in the fragment", "https://x.test/cb#code=abc123def456", "abc123def456"},
		{"activation code in the fragment", "https://x.test/cb#activation_code=abc123def456", "abc123def456"},
		{"surrounding whitespace", "  https://x.test/cb?code=abc123def456  ", "abc123def456"},
		{"the alternate parameter name", "https://x.test/cb?authorization_code=abc123def456", "abc123def456"},
		{"a link with no code", "https://x.test/welcome", ""},
		{"empty", "", ""},
		{"prose pasted by mistake", "I clicked the link and it said welcome", ""},
		{"too short to be a code", "abc", ""},
		// A code that happens to be URL-safe should survive being pasted on its
		// own without being mistaken for prose.
		{"a long opaque code", strings.Repeat("a", 200), strings.Repeat("a", 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCode(tc.input); got != tc.want {
				t.Errorf("ParseCode(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestLoginSendsTheChallengeItWillVerifyWith is the property the whole flow
// rests on. PKCE only proves anything if the challenge sent to the API in step
// one is the digest of the verifier sent back in step three; if they come from
// different sources the server rejects the exchange, and the error it gives
// blames the code.
func TestLoginSendsTheChallengeItWillVerifyWith(t *testing.T) {
	var gotChallenge, gotVerifier, gotEmail string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)

		switch r.URL.Path {
		case "/magic-link":
			gotChallenge, gotEmail = body["code_challenge"], body["email"]
			if got := body["code_challenge_method"]; got != "S256" {
				t.Errorf("code_challenge_method = %q, want S256 — the plain method would send the verifier itself", got)
			}
			w.WriteHeader(http.StatusOK)
		case "/oauth/token":
			gotVerifier = body["code_verifier"]
			if got := body["grant_type"]; got != "authorization_code" {
				t.Errorf("grant_type = %q, want authorization_code", got)
			}
			if got := body["code"]; got != "the-code-from-the-email" {
				t.Errorf("code = %q — the pasted link was not parsed", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at", "refresh_token": "rt", "expires_in": 3600,
			})
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	e := NewExchanger(srv.URL)
	tok, err := e.Login(context.Background(), "someone@example.com",
		func(string) {},
		func() (string, error) {
			return "https://mysteriumvpn.com/auth/callback?code=the-code-from-the-email", nil
		})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if gotEmail != "someone@example.com" {
		t.Errorf("magic link sent to %q", gotEmail)
	}
	if gotChallenge == "" || gotVerifier == "" {
		t.Fatal("the challenge or the verifier never reached the server")
	}
	if want := ChallengeFor(gotVerifier); gotChallenge != want {
		t.Error("the challenge sent in step one is not the digest of the verifier sent in step three — the server would reject every login")
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
		t.Errorf("token not carried through: %+v", tok)
	}
	if tok.ObtainedAt.IsZero() {
		t.Error("ObtainedAt was not set, so the token looks eternally unexpired")
	}
}

// TestLoginSaysSoWhenTheAddressHasNoAccount covers the likeliest way a login
// fails. "HTTP 404" sends someone to check their network; the address is the
// thing that is actually wrong.
func TestLoginSaysSoWhenTheAddressHasNoAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewExchanger(srv.URL).Login(context.Background(), "nobody@example.com",
		func(string) {}, func() (string, error) { return "unused", nil })
	if err == nil {
		t.Fatal("a 404 was reported as success")
	}
	if !strings.Contains(err.Error(), "no account") {
		t.Errorf("error does not name the likely cause: %v", err)
	}
}

// TestLoginSaysSoWhenTheLinkIsStale keeps the other common failure honest. An
// expired or already-used link is rejected as a 400, and the API's own wording
// for it does not tell the user what to do next.
func TestLoginSaysSoWhenTheLinkIsStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/magic-link" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"invalid_grant"}}`))
	}))
	defer srv.Close()

	_, err := NewExchanger(srv.URL).Login(context.Background(), "someone@example.com",
		func(string) {}, func() (string, error) { return "abc123def456", nil })
	if err == nil {
		t.Fatal("a rejected code was reported as success")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error does not explain what to do next: %v", err)
	}
}

// TestLoginRejectsAnEmptyEmail saves a pointless round trip and a confusing
// error from the API.
func TestLoginRejectsAnEmptyEmail(t *testing.T) {
	_, err := NewExchanger("http://127.0.0.1:1").Login(context.Background(), "   ",
		func(string) {}, func() (string, error) { return "", nil })
	if err == nil || !strings.Contains(err.Error(), "email") {
		t.Errorf("err = %v, want one that asks for an email address", err)
	}
}
