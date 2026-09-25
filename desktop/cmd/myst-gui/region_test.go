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
	list := regions.All

	// Row 0 is the "Fastest" entry: no country, let the API choose.
	if got := regionCodeAt(list, 0); got != "" {
		t.Errorf("regionCodeAt(0) = %q, want the empty country", got)
	}
	// A fresh GtkDropDown reports -1 before anything is selected. It has to
	// mean the same thing as row 0 rather than a negative index into the list.
	if got := regionCodeAt(list, -1); got != "" {
		t.Errorf("regionCodeAt(-1) = %q, want the empty country", got)
	}
	if got := regionRow(list, ""); got != 0 {
		t.Errorf("regionRow(\"\") = %d, want 0", got)
	}

	// Row n+1 must be region n — the off-by-one that matters.
	for i, r := range list {
		if got := regionCodeAt(list, i+1); got != r.Code {
			t.Errorf("regionCodeAt(%d) = %q, want %q", i+1, got, r.Code)
		}
		if got := regionRow(list, r.Code); got != uint(i+1) {
			t.Errorf("regionRow(%q) = %d, want %d", r.Code, got, i+1)
		}
	}

	// The first country in the list sits at row 1, never row 0. Before the fix
	// the picker opened on it, which is how "us" got requested unasked.
	if len(list) > 0 {
		if first := list[0].Code; regionCodeAt(list, 0) == first {
			t.Errorf("row 0 resolves to %q — the picker would default to a country the user never chose", first)
		}
	}

	// Past the end is not a country.
	if got := regionCodeAt(list, len(list)+1); got != "" {
		t.Errorf("regionCodeAt(one past the end) = %q, want the empty country", got)
	}
}

// TestRegionMappingFollowsTheList is the property the picker gained when the
// country list stopped being a built-in constant.
//
// The list now comes from the daemon and can be any length. If the offset were
// still tied to the compiled-in fallback, a longer list would map every row
// past the fallback's length to the wrong country, or to none.
func TestRegionMappingFollowsTheList(t *testing.T) {
	short := []regions.Region{{Code: "GB", Name: "United Kingdom"}}
	long := []regions.Region{
		{Code: "AR", Name: "Argentina"},
		{Code: "BR", Name: "Brazil"},
		{Code: "PF", Name: "French Polynesia"},
		{Code: "ZA", Name: "South Africa"},
	}

	for name, list := range map[string][]regions.Region{"one entry": short, "four entries": long} {
		t.Run(name, func(t *testing.T) {
			if got := regionCodeAt(list, 0); got != "" {
				t.Errorf("row 0 = %q, want the empty country", got)
			}
			for i, r := range list {
				if got := regionCodeAt(list, i+1); got != r.Code {
					t.Errorf("row %d = %q, want %q", i+1, got, r.Code)
				}
				if got := regionRow(list, r.Code); got != uint(i+1) {
					t.Errorf("regionRow(%q) = %d, want %d", r.Code, got, i+1)
				}
			}
			// One past this list's end, not the fallback's.
			if got := regionCodeAt(list, len(list)+1); got != "" {
				t.Errorf("row %d = %q, want the empty country", len(list)+1, got)
			}
		})
	}
}

// TestRegionRowIsCaseInsensitive covers the status path. The daemon reports the
// API's canonical code, and the picker has to find it whatever casing it uses,
// or a connected country would silently display as "Fastest".
func TestRegionRowIsCaseInsensitive(t *testing.T) {
	list := []regions.Region{{Code: "GB", Name: "United Kingdom"}}
	if got := regionRow(list, "gb"); got != 1 {
		t.Errorf("regionRow(\"gb\") = %d, want 1", got)
	}
	if got := regionRow(list, "GB"); got != 1 {
		t.Errorf("regionRow(\"GB\") = %d, want 1", got)
	}
	// A country that is not in the list falls back to "Fastest" rather than to
	// a wrong row.
	if got := regionRow(list, "ZZ"); got != 0 {
		t.Errorf("regionRow(\"ZZ\") = %d, want 0", got)
	}
}
