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
	variants := []string{
		first.Code,
		strings.ToLower(first.Code),
		strings.ToUpper(first.Code[:1]) + strings.ToLower(first.Code[1:]), // "Gb"
	}
	for _, variant := range variants {
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

// advertisedByAPI is the country list GET /connection/config returned when the
// name table was written. Embedded as a fixture so the test knows what the API
// actually offers, rather than only checking the table against itself.
const advertisedByAPI = "AD AE AL AR AT AU AZ BA BB BD BE BG BH BR BT BY CA CH CL CO CR CY CZ " +
	"DE DK DO DZ EC EE EG ES FI FR GB GE GP GR GT GY HK HR HU ID IE IL IN IT JE JO JP KE KG " +
	"KH KR KZ LA LK LT LU LV MA MD MK MN MO MQ MT MV MX MY NG NL NO NZ PA PE PF PH PK PL PR " +
	"PT PY RO RS RU SA SE SG SI SK SR TH TR TT TW TZ UA US UY UZ VE VN ZA"

// TestEveryAdvertisedCountryHasAName guards the gap the picker would show
// otherwise: a country listed as a bare code reads as a bug, and the whole
// point of the table is that a person never sees "PF".
func TestEveryAdvertisedCountryHasAName(t *testing.T) {
	for _, code := range strings.Fields(advertisedByAPI) {
		name, ok := Names[code]
		if !ok {
			t.Errorf("the API offers %s but there is no display name for it", code)
			continue
		}
		if name == "" || name == code {
			t.Errorf("country %s has no real name (got %q)", code, name)
		}
	}
}

// TestFallbackIsASubsetOfTheNames keeps the fallback list honest. It is shown
// when the daemon cannot be reached, so every entry must be one the API
// actually serves — otherwise the fallback would offer countries that cannot
// connect.
func TestFallbackIsASubsetOfTheNames(t *testing.T) {
	advertised := map[string]bool{}
	for _, c := range strings.Fields(advertisedByAPI) {
		advertised[c] = true
	}
	if len(All) == 0 {
		t.Fatal("the fallback list is empty")
	}
	for _, r := range All {
		if !advertised[r.Code] {
			t.Errorf("fallback offers %s, which the API does not list", r.Code)
		}
		if r.Name == r.Code {
			t.Errorf("fallback entry %s has no name", r.Code)
		}
	}
}

// TestFromCodesNormalisesAndSorts covers the path from the daemon's answer to
// something a picker can render.
func TestFromCodesNormalisesAndSorts(t *testing.T) {
	got := FromCodes([]string{"za", " ZA ", "es", "gb"})
	// By display name, not by code: "South Africa" sorts before "Spain"
	// because 'o' precedes 'p'. By code the order would be ES, GB, ZA.
	want := []string{"ZA", "ES", "GB"}
	if len(got) != len(want) {
		t.Fatalf("FromCodes returned %d regions, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Code != want[i] {
			t.Errorf("FromCodes()[%d].Code = %q, want %q", i, got[i].Code, want[i])
		}
	}
	if got[0].Name != "South Africa" || got[1].Name != "Spain" || got[2].Name != "United Kingdom" {
		t.Errorf("names not resolved: %v", got)
	}
	// Sorted by display name, which is the order a person expects to read.
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Errorf("not sorted by name: %q before %q", got[i-1].Name, got[i].Name)
		}
	}
	// An empty answer is nil, not an empty slice: a caller needs to tell
	// "the daemon told us nothing" from "we are showing nothing".
	if FromCodes(nil) != nil {
		t.Error("FromCodes(nil) should be nil")
	}
}
