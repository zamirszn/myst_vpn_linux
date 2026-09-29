package regions

import "testing"

func sample() []RawCountry {
	return []RawCountry{
		{Code: "us", Name: "United States"},
		{Code: "GB", Name: "United Kingdom", Cities: []RawCity{
			{Slug: "manchester", Name: "Manchester"},
			{Slug: "london", Name: "London"},
			{Slug: "london", Name: "London again"}, // duplicate slug
			{Slug: "", Name: "nothing to connect to"},
			{Slug: "st-albans"}, // no name from the API
		}},
		{Code: "  "},            // nothing to connect to
		{Code: "GB", Name: "x"}, // duplicate country
		{Code: "ZZ"},            // unknown code, no name
	}
}

func TestFromLocationsNormalisesAndSorts(t *testing.T) {
	got := FromLocations(sample())

	// Sorted by display name, codes upper-cased, empty and duplicate entries gone.
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	want := []string{"United Kingdom", "United States", "ZZ"}
	if len(names) != len(want) {
		t.Fatalf("places = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("places = %v, want %v", names, want)
		}
	}
	if got[1].Code != "US" {
		t.Errorf("code = %q, want it upper-cased", got[1].Code)
	}

	uk := got[0]
	var slugs []string
	for _, c := range uk.Cities {
		slugs = append(slugs, c.Slug+"="+c.Name)
	}
	wantCities := []string{"london=London", "manchester=Manchester", "st-albans=St-Albans"}
	if len(slugs) != len(wantCities) {
		t.Fatalf("cities = %v, want %v", slugs, wantCities)
	}
	for i := range wantCities {
		if slugs[i] != wantCities[i] {
			t.Fatalf("cities = %v, want %v", slugs, wantCities)
		}
	}
	if len(got[1].Cities) != 0 {
		t.Errorf("a country listed without cities must stay without them, got %v", got[1].Cities)
	}
}

func TestFromLocationsKeepsSlugCaseVerbatim(t *testing.T) {
	got := FromLocations([]RawCountry{{Code: "gb", Cities: []RawCity{{Slug: " Gloucester ", Name: "Gloucester"}}}})
	if s := got[0].Cities[0].Slug; s != "Gloucester" {
		t.Errorf("slug = %q, want only whitespace trimmed and the case left alone", s)
	}
}

func TestFromLocationsEmptyIsNil(t *testing.T) {
	if FromLocations(nil) != nil || FromLocations([]RawCountry{{Code: ""}}) != nil {
		t.Error("nothing to show must be nil, not an empty non-nil list")
	}
}

func TestCitiesOfAndCityName(t *testing.T) {
	places := FromLocations(sample())
	if n := len(CitiesOf(places, "gb")); n != 3 {
		t.Errorf("CitiesOf(gb) = %d cities, want 3 (case-insensitive)", n)
	}
	if CitiesOf(places, "ZZ") != nil || CitiesOf(places, "FR") != nil {
		t.Error("a country without cities, or an unknown one, has none")
	}
	if got := CityName(places, "GB", "LONDON"); got != "London" {
		t.Errorf("CityName = %q, want the API's name regardless of case", got)
	}
	// Not in the list (say, a status shown before the list loaded).
	if got := CityName(nil, "GB", "gloucester"); got != "Gloucester" {
		t.Errorf("CityName fallback = %q, want a title-cased slug", got)
	}
	if got := CityName(places, "GB", ""); got != "" {
		t.Errorf("CityName of no city = %q, want empty", got)
	}
}

func TestCityRowAndSlugRoundTrip(t *testing.T) {
	cities := FromLocations(sample())[0].Cities

	for _, row := range []int{-1, 0} {
		if got := CitySlugAt(cities, row); got != "" {
			t.Errorf("CitySlugAt(%d) = %q, want no city (\"Any city\")", row, got)
		}
	}
	if got := CityRow(cities, ""); got != 0 {
		t.Errorf("CityRow(\"\") = %d, want 0", got)
	}
	for i, c := range cities {
		if got := CitySlugAt(cities, i+1); got != c.Slug {
			t.Errorf("CitySlugAt(%d) = %q, want %q", i+1, got, c.Slug)
		}
		if got := CityRow(cities, c.Slug); got != uint(i+1) {
			t.Errorf("CityRow(%q) = %d, want %d", c.Slug, got, i+1)
		}
	}
	if got := CitySlugAt(cities, len(cities)+1); got != "" {
		t.Errorf("one past the end = %q, want no city", got)
	}
	if got := CityRow(cities, "nowhere"); got != 0 {
		t.Errorf("CityRow(unknown) = %d, want 0", got)
	}
	if got := CityRow(cities, "LONDON"); got != 1 {
		t.Errorf("CityRow is case-insensitive for the status path, got %d", got)
	}
}
