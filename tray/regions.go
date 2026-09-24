package main

import (
	"strings"
	"unicode"
)

// region is one entry in the Region submenu.
type region struct {
	code string
	name string
}

// regions is a curated set rather than the account's full country list.
//
// The complete list lives behind GET /connection/config, which needs an access
// token — and the token deliberately belongs to the daemon, not to a desktop
// applet running as the logged-in user. Fetching it here would mean a second
// copy of the credential, so the applet offers the countries people actually
// pick and accepts any code the user sets with -country.
//
// Mysterium accepts country codes it has no nodes for; the connect simply fails
// and the failure lands in the menu's status line.
var regions = []region{
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

// regionName turns a country code into something readable. An unknown code is
// not an error — it may well be a valid country this list does not carry — so
// it is shown as-is rather than hidden.
func regionName(code string) string {
	if code == "" {
		return "the fastest region"
	}
	for _, r := range regions {
		if strings.EqualFold(r.code, code) {
			return r.name
		}
	}
	return strings.ToUpper(code)
}

// titleCase capitalises the first letter of each word, which is all the daemon's
// lowercase city names ("gloucester") need.
func titleCase(s string) string {
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
