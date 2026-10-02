package daemonclient

import (
	"encoding/json"
	"testing"
)

// TestSignedOutIncludesUnknownAccountState ensures an older daemon cannot leave
// the GUI on the VPN screen while hiding login/logout controls.
func TestSignedOutIncludesUnknownAccountState(t *testing.T) {
	cases := []struct {
		name string
		wire string
		want bool
	}{
		{"an older daemon that does not report account state", `{"state":"disconnected"}`, true},
		{"signed in", `{"state":"disconnected","logged_in":true,"account":"a@b.co"}`, false},
		{"signed out", `{"state":"disconnected","logged_in":false}`, true},
	}
	for _, tc := range cases {
		var st Status
		if err := json.Unmarshal([]byte(tc.wire), &st); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := st.SignedOut(); got != tc.want {
			t.Errorf("%s: SignedOut() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAccountIsDecoded checks the field names against the daemon's wire format.
func TestAccountIsDecoded(t *testing.T) {
	var st Status
	if err := json.Unmarshal([]byte(`{"state":"connected","logged_in":true,"account":"a@b.co"}`), &st); err != nil {
		t.Fatal(err)
	}
	if st.Account != "a@b.co" {
		t.Errorf("Account = %q, want a@b.co", st.Account)
	}
}
