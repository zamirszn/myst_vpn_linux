package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrNoCredentials means no token is stored: the caller must log in.
var ErrNoCredentials = errors.New("no stored credentials — run: myst login")

// ErrRefreshRejected means the refresh token is no longer valid, so the user
// must authenticate again.
var ErrRefreshRejected = errors.New("refresh token rejected — run: myst login")

// Exchanger renews tokens against the OAuth endpoint.
type Exchanger struct {
	BaseURL string
	HTTP    *http.Client
}

// NewExchanger returns an Exchanger pointed at the production API.
func NewExchanger(baseURL string) *Exchanger {
	if baseURL == "" {
		baseURL = "https://api.mysteriumvpn.com/api/v1"
	}
	return &Exchanger{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Refresh exchanges a refresh token for a new token set.
//
// The response may or may not rotate the refresh token; when it does not, the
// existing one is carried forward so the client is not logged out.
func (e *Exchanger) Refresh(ctx context.Context, current *Token) (*Token, error) {
	if current == nil || current.RefreshToken == "" {
		return nil, ErrNoCredentials
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     ClientID,
		"refresh_token": current.RefreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("encode refresh request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest {
		return nil, ErrRefreshRejected
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("refresh failed: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var out Token
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("refresh response contained no access_token")
	}

	// Carry forward what the server did not resend.
	if out.RefreshToken == "" {
		out.RefreshToken = current.RefreshToken
	}
	if out.TokenType == "" {
		out.TokenType = current.TokenType
	}
	out.ObtainedAt = time.Now()
	return &out, nil
}

// EnsureValid returns a usable token, refreshing first if it is close to expiry.
func (e *Exchanger) EnsureValid(ctx context.Context, store Store, margin time.Duration) (*Token, error) {
	tok, err := store.Load()
	if err != nil {
		return nil, err
	}
	if !tok.Expired(margin) {
		return tok, nil
	}

	refreshed, err := e.Refresh(ctx, tok)
	if err != nil {
		return nil, err
	}
	if err := store.Save(refreshed); err != nil {
		// The token is valid; failing to persist it should not fail the call.
		// Worst case we refresh again next time.
		return refreshed, nil
	}
	return refreshed, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
