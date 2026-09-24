package main

import (
	"testing"

	"github.com/zamirszn/mysterium-linux-client/desktop/internal/regions"
)

// TestRegionRowAndCodeRoundTrip is the regression for the bug that made a
// first-run click on Connect fail.
//
// The picker's row 0 is "Fastest" and the regions start at row 1, so the two
// mappings carry an offset. Getting it wrong does not crash: it silently
// connects the user to the wrong country, or — as happened with the US —
// asks for a region nobody chose and fails because it has no free node.
func TestRegionRowAndCodeRoundTrip(t *testing.T) {
	// Row 0 is the "Fastest" entry: no country, let the API choose.
	if got := regionCodeAt(0); got != "" {
		t.Errorf("regionCodeAt(0) = %q, want the empty country", got)
	}
	// A fresh GtkDropDown reports -1 before anything is selected. It has to
	// mean the same thing as row 0 rather than a negative index into the list.
	if got := regionCodeAt(-1); got != "" {
		t.Errorf("regionCodeAt(-1) = %q, want the empty country", got)
	}
	if got := regionRow(""); got != 0 {
		t.Errorf("regionRow(\"\") = %d, want 0", got)
	}

	// Row n+1 must be region n — the off-by-one that matters.
	for i, r := range regions.All {
		if got := regionCodeAt(i + 1); got != r.Code {
			t.Errorf("regionCodeAt(%d) = %q, want %q", i+1, got, r.Code)
		}
		if got := regionRow(r.Code); got != uint(i+1) {
			t.Errorf("regionRow(%q) = %d, want %d", r.Code, got, i+1)
		}
	}

	// The first country in the list sits at row 1, never row 0. Before the fix
	// the picker opened on it, which is how "us" got requested unasked.
	if len(regions.All) > 0 {
		if first := regions.All[0].Code; regionCodeAt(0) == first {
			t.Errorf("row 0 resolves to %q — the picker would default to a country the user never chose", first)
		}
	}

	// Past the end is not a country.
	if got := regionCodeAt(len(regions.All) + 1); got != "" {
		t.Errorf("regionCodeAt(one past the end) = %q, want the empty country", got)
	}
}
