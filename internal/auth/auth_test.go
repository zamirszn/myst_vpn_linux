package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenExpiry(t *testing.T) {
	now := time.Now()

	fresh := &Token{ObtainedAt: now, ExpiresIn: 3600}
	if fresh.Expired(time.Minute) {
		t.Error("a token valid for an hour should not be expired")
	}
	if got := fresh.ExpiresAt(); got.Sub(now) != time.Hour {
		t.Errorf("ExpiresAt = %v, want now+1h", got)
	}

	stale := &Token{ObtainedAt: now.Add(-2 * time.Hour), ExpiresIn: 3600}
	if !stale.Expired(time.Minute) {
		t.Error("a token that lapsed an hour ago should be expired")
	}

	// The margin must treat a nearly-expired token as expired, so we never hand
	// a token to a request that will outlive it.
	nearly := &Token{ObtainedAt: now, ExpiresIn: 30}
	if !nearly.Expired(time.Minute) {
		t.Error("a token expiring in 30s should be treated as expired with a 1m margin")
	}

	// An unknown lifetime should defer to the server rather than forcing a
	// refresh on every single call.
	unknown := &Token{}
	if unknown.Expired(time.Minute) {
		t.Error("a token with unknown lifetime should not be reported as expired")
	}
}

func TestStoreRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "token.json")
	store := Store{Path: path}

	if _, err := store.Load(); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("Load on a missing file = %v, want ErrNoCredentials", err)
	}

	want := &Token{AccessToken: "access-abc", RefreshToken: "refresh-xyz", ExpiresIn: 3600}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("round trip mismatch: got %+v", got)
	}
	if got.ObtainedAt.IsZero() {
		t.Error("Save should stamp ObtainedAt when unset")
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	store := Store{Path: filepath.Join(dir, "token.json")}
	for i := 0; i < 3; i++ {
		if err := store.Save(&Token{AccessToken: "a", RefreshToken: "r", ExpiresIn: 60}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "token.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temp files left behind: %v", names)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "token.json")}
	if err := store.Delete(); err != nil {
		t.Errorf("deleting a missing token should not error, got %v", err)
	}
	if err := store.Save(&Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("Load after Delete = %v, want ErrNoCredentials", err)
	}
}

func TestRefreshRotatesAndCarriesForward(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		// Deliberately omit refresh_token: the server did not rotate it, and the
		// client must keep using the existing one rather than logging out.
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access",
			"token_type":   "bearer",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	ex := NewExchanger(srv.URL)
	got, err := ex.Refresh(context.Background(), &Token{RefreshToken: "old-refresh"})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotBody["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotBody["grant_type"])
	}
	if gotBody["refresh_token"] != "old-refresh" {
		t.Errorf("sent refresh_token = %q, want old-refresh", gotBody["refresh_token"])
	}
	if got.AccessToken != "new-access" {
		t.Errorf("AccessToken = %q, want new-access", got.AccessToken)
	}
	if got.RefreshToken != "old-refresh" {
		t.Errorf("RefreshToken = %q, want it carried forward as old-refresh", got.RefreshToken)
	}
	if got.ObtainedAt.IsZero() {
		t.Error("refreshed token should carry a fresh ObtainedAt")
	}
}

func TestRefreshRejectedOnUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewExchanger(srv.URL).Refresh(context.Background(), &Token{RefreshToken: "dead"})
	if !errors.Is(err, ErrRefreshRejected) {
		t.Errorf("Refresh on 401 = %v, want ErrRefreshRejected", err)
	}
}

func TestRefreshWithoutToken(t *testing.T) {
	_, err := NewExchanger("http://127.0.0.1:1").Refresh(context.Background(), &Token{})
	if !errors.Is(err, ErrNoCredentials) {
		t.Errorf("Refresh with no refresh token = %v, want ErrNoCredentials", err)
	}
}

func TestEnsureValidOnlyRefreshesWhenNeeded(t *testing.T) {
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "refresh_token": "r2", "expires_in": 3600})
	}))
	defer srv.Close()

	store := Store{Path: filepath.Join(t.TempDir(), "token.json")}
	ex := NewExchanger(srv.URL)

	// A healthy token must not trigger a refresh.
	if err := store.Save(&Token{AccessToken: "good", RefreshToken: "r1", ExpiresIn: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.EnsureValid(context.Background(), store, time.Minute); err != nil {
		t.Fatalf("EnsureValid: %v", err)
	}
	if refreshes != 0 {
		t.Errorf("refreshed %d times for a healthy token, want 0", refreshes)
	}

	// An expired one must, and the result must be persisted.
	if err := store.Save(&Token{AccessToken: "old", RefreshToken: "r1", ExpiresIn: 1}); err != nil {
		t.Fatal(err)
	}
	tok, err := ex.EnsureValid(context.Background(), store, time.Minute)
	if err != nil {
		t.Fatalf("EnsureValid: %v", err)
	}
	if refreshes != 1 {
		t.Errorf("refreshed %d times for an expired token, want 1", refreshes)
	}
	if tok.AccessToken != "fresh" {
		t.Errorf("AccessToken = %q, want fresh", tok.AccessToken)
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AccessToken != "fresh" {
		t.Error("refreshed token was not persisted")
	}
}
