package main

import "testing"

func TestFlagEmoji(t *testing.T) {
	cases := map[string]string{
		"GB": "🇬🇧",
		"us": "🇺🇸",
		" DE ": "🇩🇪",
		"":     "🌐",
		"USA":  "🌐",
		"1A":   "🌐",
		"ZZ":   "🇿🇿", // syntactically valid even though no such country exists
	}
	for code, want := range cases {
		if got := flagEmoji(code); got != want {
			t.Errorf("flagEmoji(%q) = %q, want %q", code, got, want)
		}
	}
}
