package regions

import (
	"sort"
	"strings"
)

// City is one exit city inside a country.
type City struct {
	// Slug is what the connect request takes, exactly as the API spelled it.
	// It is never case-folded: the API matches slugs literally, and a slug in
	// the wrong case comes back as "no available instance", which reads like a
	// city with no free node.
	Slug string

	// Name is what a person sees.
	Name string
}

// Place is a country together with the cities the account can exit through in
// it. Cities is empty for a country the API lists without any, which is still
// a valid choice: the country on its own.
type Place struct {
	Region
	Cities []City
}

// RawCity and RawCountry restate the daemon's location answer without importing
// the client package, so this package stays a pure naming helper and the
// front ends convert at their own boundary.
type RawCity struct{ Slug, Name string }

type RawCountry struct {
	Code, Name string
	Cities     []RawCity
}

// FromLocations builds the picker's data from what the daemon reported: places
// sorted by display name, each with its cities sorted the same way.
//
// Sorted here rather than trusted from the API for the reason FromCodes is:
// the API's order is by code (or arbitrary), which reads as random in a menu.
//
// Codes are upper-cased because that is the only form the API matches; slugs
// are only trimmed. Duplicates and entries with nothing to connect to are
// dropped. An empty input returns nil so a caller can tell "nothing to show"
// from "showing nothing".
func FromLocations(in []RawCountry) []Place {
	if len(in) == 0 {
		return nil
	}
	out := make([]Place, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		code := strings.ToUpper(strings.TrimSpace(c.Code))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true

		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = Name(code)
		}
		out = append(out, Place{
			Region: Region{Code: code, Name: name},
			Cities: sortCities(c.Cities),
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortCities(in []RawCity) []City {
	if len(in) == 0 {
		return nil
	}
	out := make([]City, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		slug := strings.TrimSpace(c.Slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = TitleCase(slug)
		}
		out = append(out, City{Slug: slug, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CitiesOf returns the cities listed for a country code, matched
// case-insensitively, or nil when the country has none or is unknown.
func CitiesOf(places []Place, code string) []City {
	for _, p := range places {
		if strings.EqualFold(p.Code, code) {
			return p.Cities
		}
	}
	return nil
}

// CityName turns a city slug into something readable. It prefers the name the
// API gave, and falls back to title-casing the slug for a city the list does
// not know — for instance one shown from a status before the list has loaded.
func CityName(places []Place, code, slug string) string {
	if slug == "" {
		return ""
	}
	for _, c := range CitiesOf(places, code) {
		if strings.EqualFold(c.Slug, slug) {
			return c.Name
		}
	}
	return TitleCase(slug)
}

// CitySlugAt maps a city picker row to a slug, and CityRow does the reverse.
//
// Row 0 is the leading "Any city" entry, so the cities start at row 1. Like the
// country mappings they take the list rather than reading state, so they are
// testable without a display, and because an off-by-one here connects the user
// to the wrong city without any error to show for it. A negative row, which is
// what a fresh GtkDropDown reports, means the same as row 0.
func CitySlugAt(cities []City, row int) string {
	if row <= 0 || row > len(cities) {
		return ""
	}
	return cities[row-1].Slug
}

// CityRow is the inverse of CitySlugAt. A slug not in the list maps to row 0,
// "Any city", rather than to a wrong row.
func CityRow(cities []City, slug string) uint {
	if slug == "" {
		return 0
	}
	for i, c := range cities {
		if strings.EqualFold(c.Slug, slug) {
			return uint(i + 1)
		}
	}
	return 0
}
