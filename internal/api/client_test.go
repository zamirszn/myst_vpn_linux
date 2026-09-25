package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The exact detail string observed from a live call asking for a country with
// no free node, with the account's own user id replaced by a placeholder.
//
// Kept verbatim in every other respect — same words, same punctuation, same
// field order and spacing — because the whole value of this predicate is that
// it matches what the server actually sends, and a tidy-looking approximation
// would prove nothing.
const liveNoServerDetail = "Could not retrieve wireguard connect configuration: " +
	"failed to find available wireguard server: no available instance for " +
	"filters: {Country:us City: IPType: UserID:00000000-0000-0000-0000-000000000000 " +
	"UserPlanID:plan_example HashSalt:}"

func TestIsNoServerInRegion(t *testing.T) {
	cases := []struct {
		name string
		err  APIError
		want bool
	}{
		{
			// The live failure. Before this predicate existed it surfaced as
			// "api 500 2332: Could not retrieve wireguard connect
			// configuration", which reads like a server fault and gives the
			// user nothing to do.
			name: "the observed no-node-in-region failure",
			err: APIError{
				Status:  http.StatusInternalServerError,
				Code:    "2332",
				Message: "Could not retrieve wireguard connect configuration",
				Detail:  liveNoServerDetail,
			},
			want: true,
		},
		{
			// A genuine server fault, which must NOT be reported to the user as
			// "that country is unavailable" — the advice would be wrong.
			name: "a real server fault",
			err: APIError{
				Status:  http.StatusInternalServerError,
				Code:    "2320",
				Message: "Internal server error",
				Detail:  "something exploded in the planner",
			},
			want: false,
		},
		{
			// The device-slot condition is also an HTTP 500 with a populated
			// detail, so the two must not be confused.
			name: "the device-slot condition",
			err: APIError{
				Status:  http.StatusInternalServerError,
				Code:    "2333",
				Message: "no device slots",
				Detail:  "allowed_devices_limit reached",
			},
			want: false,
		},
		{name: "empty error", err: APIError{}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.IsNoServerInRegion(); got != tc.want {
				t.Errorf("IsNoServerInRegion() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAPIErrorStillReportsTheRawBody pins the fallback. The friendly message is
// added by the daemon, which knows which country was asked for; the API layer
// must keep carrying the server's own words so that anything unrecognised
// still surfaces intact instead of being swallowed.
func TestAPIErrorStillReportsTheRawBody(t *testing.T) {
	err := &APIError{
		Status:  http.StatusInternalServerError,
		Code:    "2332",
		Message: "Could not retrieve wireguard connect configuration",
		Detail:  liveNoServerDetail,
	}
	const want = "api 500 2332: Could not retrieve wireguard connect configuration"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestConnectNormalisesCountry is the regression for the bug that made every
// region selection fail while "fastest" worked.
//
// The API matches country filters case-sensitively against uppercase ISO
// codes, and a code that does not match is not ignored — the whole request
// fails with "no available instance for filters", which reads like the country
// having no free node. A live probe showed the difference exactly:
//
//	country=gb -> HTTP 500 (code 2332)   country=GB -> HTTP 200
//	country=de -> HTTP 500 (code 2332)   country=DE -> HTTP 200
//
// So the fix belongs at the wire boundary, where every caller passes through.
func TestConnectNormalisesCountry(t *testing.T) {
	cases := []struct {
		sent string
		want string
	}{
		{"gb", "GB"},   // what the CLI documents and the old list held
		{" GB ", "GB"}, // padded
		{"Gb", "GB"},   // mixed
		{"GB", "GB"},   // already canonical
		{"", ""},       // no country: must stay absent, not become something
	}

	for _, tc := range cases {
		t.Run("sent "+tc.sent, func(t *testing.T) {
			var onWire string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body WireguardConnectRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				onWire = body.Country
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"x","wg_config":"[Interface]","country":"GB"}`)
			}))
			defer srv.Close()

			c := New("test-token")
			c.BaseURL = srv.URL
			if _, err := c.Connect(context.Background(), WireguardConnectRequest{
				PublicKey: "pub", Country: tc.sent,
			}); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if onWire != tc.want {
				t.Errorf("country on the wire = %q, want %q", onWire, tc.want)
			}
		})
	}
}
