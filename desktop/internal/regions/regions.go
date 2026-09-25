// Package regions holds the country names the desktop front ends display, and
// the small formatting helpers that go with them.
//
// The list of countries an account can actually reach is not here: it lives
// behind an authenticated endpoint, so it comes from the daemon over the
// control socket. What is here is the naming — a code on its own ("PF") is not
// something to put in front of a person — plus a short fallback list for when
// the daemon cannot be reached.
package regions

import (
	"sort"
	"strings"
	"unicode"
)

// Region is one entry in the Region submenu.
type Region struct {
	Code string
	Name string
}

// Names maps ISO 3166-1 alpha-2 codes to display names.
//
// Covers every country the API advertised in GET /connection/config when this
// was written. An entry here is only a label: the API decides what is actually
// connectable, and a code it does not serve fails at connect time.
//
// Codes are UPPERCASE because that is the only form the API matches. A
// lowercase code is not rejected as malformed — it comes back as HTTP 500 with
// "no available instance for filters", which reads exactly like the country
// having no free node. Every region code in this project was lowercase, so
// every country selection failed while "fastest" kept working, and the error
// pointed at server capacity rather than at us.
var Names = map[string]string{
	"AD": "Andorra",
	"AE": "United Arab Emirates",
	"AL": "Albania",
	"AR": "Argentina",
	"AT": "Austria",
	"AU": "Australia",
	"AZ": "Azerbaijan",
	"BA": "Bosnia and Herzegovina",
	"BB": "Barbados",
	"BD": "Bangladesh",
	"BE": "Belgium",
	"BG": "Bulgaria",
	"BH": "Bahrain",
	"BR": "Brazil",
	"BT": "Bhutan",
	"BY": "Belarus",
	"CA": "Canada",
	"CH": "Switzerland",
	"CL": "Chile",
	"CO": "Colombia",
	"CR": "Costa Rica",
	"CY": "Cyprus",
	"CZ": "Czechia",
	"DE": "Germany",
	"DK": "Denmark",
	"DO": "Dominican Republic",
	"DZ": "Algeria",
	"EC": "Ecuador",
	"EE": "Estonia",
	"EG": "Egypt",
	"ES": "Spain",
	"FI": "Finland",
	"FR": "France",
	"GB": "United Kingdom",
	"GE": "Georgia",
	"GP": "Guadeloupe",
	"GR": "Greece",
	"GT": "Guatemala",
	"GY": "Guyana",
	"HK": "Hong Kong",
	"HR": "Croatia",
	"HU": "Hungary",
	"ID": "Indonesia",
	"IE": "Ireland",
	"IL": "Israel",
	"IN": "India",
	"IT": "Italy",
	"JE": "Jersey",
	"JO": "Jordan",
	"JP": "Japan",
	"KE": "Kenya",
	"KG": "Kyrgyzstan",
	"KH": "Cambodia",
	"KR": "South Korea",
	"KZ": "Kazakhstan",
	"LA": "Laos",
	"LK": "Sri Lanka",
	"LT": "Lithuania",
	"LU": "Luxembourg",
	"LV": "Latvia",
	"MA": "Morocco",
	"MD": "Moldova",
	"MK": "North Macedonia",
	"MN": "Mongolia",
	"MO": "Macao",
	"MQ": "Martinique",
	"MT": "Malta",
	"MV": "Maldives",
	"MX": "Mexico",
	"MY": "Malaysia",
	"NG": "Nigeria",
	"NL": "Netherlands",
	"NO": "Norway",
	"NZ": "New Zealand",
	"PA": "Panama",
	"PE": "Peru",
	"PF": "French Polynesia",
	"PH": "Philippines",
	"PK": "Pakistan",
	"PL": "Poland",
	"PR": "Puerto Rico",
	"PT": "Portugal",
	"PY": "Paraguay",
	"RO": "Romania",
	"RS": "Serbia",
	"RU": "Russia",
	"SA": "Saudi Arabia",
	"SE": "Sweden",
	"SG": "Singapore",
	"SI": "Slovenia",
	"SK": "Slovakia",
	"SR": "Suriname",
	"TH": "Thailand",
	"TR": "Turkey",
	"TT": "Trinidad and Tobago",
	"TW": "Taiwan",
	"TZ": "Tanzania",
	"UA": "Ukraine",
	"US": "United States",
	"UY": "Uruguay",
	"UZ": "Uzbekistan",
	"VE": "Venezuela",
	"VN": "Vietnam",
	"ZA": "South Africa",
}

// fallbackCodes are the countries offered when the daemon cannot supply the
// real list — it is not running, or has no working token. Enough of the ones
// people actually pick to be useful, rather than all hundred and change.
var fallbackCodes = []string{
	"US", "GB", "CA", "DE", "NL", "FR", "CH", "SE", "ES",
	"IT", "PL", "RO", "JP", "SG", "AU", "BR", "IN", "ZA",
}

// All is the fallback list, with names resolved from Names so the two cannot
// disagree about how a country is spelled.
var All = func() []Region {
	out := make([]Region, 0, len(fallbackCodes))
	for _, code := range fallbackCodes {
		out = append(out, Region{Code: code, Name: Name(code)})
	}
	return out
}()

// FromCodes builds a sorted region list from the codes the daemon reported.
//
// Sorted by display name rather than by code: the API returns them in code
// order, which puts South Africa before Spain and reads as arbitrary in a
// picker. An empty list returns nil so a caller can tell "nothing to show"
// from "showing nothing".
func FromCodes(codes []string) []Region {
	if len(codes) == 0 {
		return nil
	}
	out := make([]Region, 0, len(codes))
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		code = strings.ToUpper(strings.TrimSpace(code))
		// Deduplicated: two rows for one country would be a visible oddity, and
		// the API is under no obligation to promise it will not repeat one.
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, Region{Code: code, Name: Name(code)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Name turns a country code into something readable. An unknown code is
// not an error — the account may reach a country this table has no label for —
// so it is shown as-is rather than hidden.
func Name(code string) string {
	if code == "" {
		return "the fastest region"
	}
	if name, ok := Names[strings.ToUpper(code)]; ok {
		return name
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
