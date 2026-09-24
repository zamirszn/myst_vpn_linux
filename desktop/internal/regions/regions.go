// Package regions holds the list of exit countries the desktop front ends
// offer, and the small formatting helpers that go with it.
package regions

import (
	"strings"
	"unicode"
)

// Region is one entry in the Region submenu.
type Region struct {
	Code string
	Name string
}

// All is a curated set rather than the account's full country list.
//
// The complete list lives behind GET /connection/config, which needs an access
// token — and the token deliberately belongs to the daemon, not to a desktop
// front end running as the logged-in user. Fetching it here would mean a second
// copy of the credential, so the applet offers the countries people actually
// pick and accepts any code the user sets with -country.
//
// Mysterium accepts country codes it has no nodes for; the connect simply fails
// and the failure lands in the status line.
var All = []Region{
	{"us", "United States"},
	{"gb", "United Kingdom"},
	{"ca", "Canada"},
	{"de", "Germany"},
	{"nl", "Netherlands"},
	{"fr", "France"},
	{"ch", "Switzerland"},
	{"se", "Sweden"},
	{"es", "Spain"},
	{"it", "Italy"},
	{"pl", "Poland"},
	{"ro", "Romania"},
	{"jp", "Japan"},
	{"sg", "Singapore"},
	{"au", "Australia"},
	{"br", "Brazil"},
	{"in", "India"},
	{"za", "South Africa"},
}

// Name turns a country code into something readable. An unknown code is
// not an error — it may well be a valid country this list does not carry — so
// it is shown as-is rather than hidden.
func Name(code string) string {
	if code == "" {
		return "the fastest region"
	}
	for _, r := range All {
		if strings.EqualFold(r.Code, code) {
			return r.Name
		}
	}
	return strings.ToUpper(code)
}

// TitleCase capitalises the first letter of each word, which is all the daemon's
// lowercase city names ("gloucester") need.
func TitleCase(s string) string {
	prev := ' '
	return strings.Map(func(r rune) rune {
		out := r
		if prev == ' ' || prev == '-' {
			out = unicode.ToUpper(r)
		}
		prev = r
		return out
	}, s)
}
