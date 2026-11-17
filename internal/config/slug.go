// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// maxNameLen is the maximum length of tunnel names and channel ids.
const maxNameLen = 48

// greek maps lowercase Greek letters without accents to Latin (spec 5.2).
var greek = map[rune]string{
	'α': "a", 'β': "v", 'γ': "g", 'δ': "d", 'ε': "e", 'ζ': "z", 'η': "i",
	'θ': "th", 'ι': "i", 'κ': "k", 'λ': "l", 'μ': "m", 'ν': "n", 'ξ': "x",
	'ο': "o", 'π': "p", 'ρ': "r", 'σ': "s", 'ς': "s", 'τ': "t", 'υ': "y",
	'φ': "f", 'χ': "ch", 'ψ': "ps", 'ω': "o",
}

// Slug returns the default channel id for a channel name (spec 5.2). The
// result is empty if the name has no usable characters.
func Slug(name string) string {
	// Step 1: remove accents, then map Greek letters.
	var b strings.Builder
	for _, r := range stripMarks(norm.NFD.String(name)) {
		r = unicode.ToLower(r)
		if s, ok := greek[r]; ok {
			b.WriteString(s)
		} else {
			b.WriteRune(r)
		}
	}

	// Step 2: NFKD, combining marks removed, lowercase.
	s := strings.ToLower(stripMarks(norm.NFKD.String(b.String())))

	// Step 3: runs outside [a-z0-9] become "-", trim, truncate, trim.
	b.Reset()
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	s = b.String()
	if len(s) > maxNameLen {
		s = s[:maxNameLen]
	}
	return strings.TrimRight(s, "-")
}

// stripMarks removes combining marks (Unicode category Mn).
func stripMarks(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, s)
}

// ValidName reports whether s is a valid tunnel name or channel id:
// [a-z0-9-], 1-48 characters (spec 5.2).
func ValidName(s string) bool {
	if s == "" || len(s) > maxNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
