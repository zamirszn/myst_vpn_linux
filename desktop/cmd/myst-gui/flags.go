package main

import "strings"

// flagEmoji turns an ISO 3166-1 alpha-2 code into the matching flag emoji.
// Ubuntu desktop ships Noto Color Emoji, so the picker can show a flag next to
// every country with no image assets of its own.
//
// Anything that is not exactly two ASCII letters returns a globe. That matters
// for the "Fastest" entry, whose country code is deliberately empty, and for a
// status from a daemon that reports something unexpected: a wrong flag is worse
// than a neutral one.
func flagEmoji(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 {
		return "\U0001F310" // 🌐
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return "\U0001F310"
		}
	}
	const regionalA = 0x1F1E6
	return string(rune(regionalA+int(code[0]-'A'))) + string(rune(regionalA+int(code[1]-'A')))
}
