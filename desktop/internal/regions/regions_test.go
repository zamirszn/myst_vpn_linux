package regions

import (
	"strings"
	"testing"
)

// TestCodesAreCanonical is the regression for the bug that broke every region
// selection while "fastest" kept working.
//
// The list held lowercase codes ("gb"), and the API matches country filters
// case-sensitively against uppercase ones. It does not ignore a code it cannot
// match — the whole request fails with HTTP 500, "no available instance for
// filters: {Country:gb ...}", which reads exactly like the country having no
// node free. A live probe settled it: "gb" 500, "GB" 200, "de" 500, "DE" 200.
//
// The API client normalises now, so a lowercase code here would still work —
// but it would mean the picker and the status disagreed on how to spell a
// country, which is how the picker ends up silently on the wrong row.
func TestCodesAreCanonical(t *testing.T) {
	seen := map[string]string{}
	for _, r := range All {
		if r.Code != strings.ToUpper(r.Code) {
			t.Errorf("region %q (%s) is not uppercase; the API only matches uppercase ISO codes", r.Code, r.Name)
		}
		if len(r.Code) != 2 {
			t.Errorf("region %q is not a two-letter ISO code", r.Code)
		}
		if r.Name == "" {
			t.Errorf("region %q has no display name", r.Code)
		}
		if prev, dup := seen[r.Code]; dup {
			t.Errorf("region %q appears twice: %s and %s", r.Code, prev, r.Name)
		}
		seen[r.Code] = r.Name
	}
}

// TestNameIsCaseInsensitive guards the display path. The status carries the
// API's own code, and a user can type any casing on the command line, so Name
// has to resolve all of them to the same country.
func TestNameIsCaseInsensitive(t *testing.T) {
	if len(All) == 0 {
		t.Fatal("no regions to check")
	}
	first := All[0]
	for _, variant := range []string{first.Code, strings.ToLower(first.Code), strings.Title(strings.ToLower(first.Code))} {
		if got := Name(variant); got != first.Name {
			t.Errorf("Name(%q) = %q, want %q", variant, got, first.Name)
		}
	}
	// An unknown code is shown rather than hidden: it may be a country the API
	// serves that this curated list does not carry.
	if got := Name("zz"); got != "ZZ" {
		t.Errorf("Name(\"zz\") = %q, want %q", got, "ZZ")
	}
	// No country means the API chose, which is worth saying in words.
	if got := Name(""); got != "the fastest region" {
		t.Errorf("Name(\"\") = %q, want %q", got, "the fastest region")
	}
}
