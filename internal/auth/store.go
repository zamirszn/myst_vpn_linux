// Package auth handles credential storage and renewal.
//
// A standalone client needs exactly one long-lived secret: the refresh token.
// Access tokens are short-lived and derived; the refresh token is what keeps a
// client alive without a browser, and it is the thing worth protecting.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ClientID identifies this client to the auth server.
//
// "extension" is the closest fit among the closed set the API accepts
// (web | app | extension | router) for a non-official desktop client. It is a
// placeholder to settle with Mysterium before distributing anything — shipping
// while claiming to be the Chrome extension is not honest labelling.
const ClientID = "extension"

// Token is an OAuth token set as returned by POST /oauth/token.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	UserID       string `json:"user_id,omitempty"`

	// ObtainedAt is set locally, since the server reports a relative lifetime.
	ObtainedAt time.Time `json:"obtained_at"`
}

// ExpiresAt is when the access token stops being valid.
func (t *Token) ExpiresAt() time.Time {
	if t.ObtainedAt.IsZero() || t.ExpiresIn == 0 {
		return time.Time{}
	}
	return t.ObtainedAt.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// Expired reports whether the access token is past (or near) its lifetime.
// The margin avoids handing a token to a request that will outlive it.
func (t *Token) Expired(margin time.Duration) bool {
	exp := t.ExpiresAt()
	if exp.IsZero() {
		return false // unknown lifetime; let the API decide
	}
	return time.Now().Add(margin).After(exp)
}

// Store persists a Token to disk.
type Store struct{ Path string }

// DefaultPath is where the daemon keeps its credentials. It sits under the
// daemon's own state directory rather than a user's home, because the daemon
// runs as root.
const DefaultPath = "/var/lib/mystd/token.json"

// Load reads the token. A missing file is reported distinctly so callers can
// prompt for login rather than showing an I/O error.
func (s Store) Load() (*Token, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, fmt.Errorf("read token: %w", err)
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse token %s: %w", s.Path, err)
	}
	return &t, nil
}

// Save writes the token atomically with 0600 permissions.
//
// Atomic because a crash midway through a refresh must not leave a truncated
// token file: that would log the user out and require a fresh magic link.
func (s Store) Save(t *Token) error {
	if t.ObtainedAt.IsZero() {
		t.ObtainedAt = time.Now()
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp token: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp token: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp token: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("replace token: %w", err)
	}
	return nil
}

// Delete removes stored credentials, logging the client out.
func (s Store) Delete() error {
	if err := os.Remove(s.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete token: %w", err)
	}
	return nil
}
