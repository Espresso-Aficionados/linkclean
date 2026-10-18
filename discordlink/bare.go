package discordlink

import "unicode/utf8"

// bareEnd applies Discord's bare-link rule to s[i:hi], where i is the start
// of a candidate scheme, and returns the end of the URL, or -1 if there is no
// bare link at i. Find and Repost's WrapNone check share it.
//
// The rule is discord.py's _URL_REGEX without steam://,
//
//	https?:\/\/[^\s<]+[^<.,:;"'\]\s]
//
// with \s the JavaScript set (isJSSpace) and a lowercase scheme only. Then,
// while the URL ends in ')' and holds more ')' than '(', the ')' is dropped
// and the last-character rule is applied again (unverified: Discord may not
// balance parentheses).
//
//  1. Runes after "://" are taken while they are neither JS whitespace nor
//     '<', up to hi.
//  2. The URL ends after the last of those runes, at or after the second,
//     that is not one of . , : ; " ' ]. So '>', ')', '|', '`', '*', '_', '~'
//     and '\' can end a URL; U+200B and U+0085 are not whitespace and stay
//     inside it; U+00A0, U+3000 and U+FEFF end it.
//  3. Trimming an unbalanced ')' repeats step 2 on what is left. If no rune
//     qualifies any more, there is no link: every URL bareEnd reports still
//     matches the regex above.
//
// Runes are decoded with utf8.DecodeRuneInString, so a byte of invalid UTF-8
// is a one-byte rune that is neither whitespace nor excluded. The second
// "character" is counted in runes; JavaScript counts UTF-16 units, so it
// would also accept a single astral rune after "://" (a one-rune host that
// cannot carry a tracking parameter).
//
// The cost is O(e-i), where e is the end of step 1: step 2 and the trimming
// walk back over excluded runes and dropped ')' only.
func bareEnd(s string, i, hi int) int {
	var p int
	switch {
	case hi-i >= 8 && s[i:i+8] == "https://":
		p = i + 8
	case hi-i >= 7 && s[i:i+7] == "http://":
		p = i + 7
	default:
		return -1
	}
	first := -1 // end of the first rune after "://"
	end := -1   // end of the last rune that may end the URL
	opens, closes := 0, 0
	eo, ec := 0, 0 // opens and closes in s[i:end]
	for p < hi {
		r, n := utf8.DecodeRuneInString(s[p:hi])
		if r == '<' || isJSSpace(r) {
			break
		}
		p += n
		switch r {
		case '(':
			opens++
		case ')':
			closes++
		}
		if first < 0 {
			first = p
			continue
		}
		if !bareExcluded(r) {
			end, eo, ec = p, opens, closes
		}
	}
	if end < 0 {
		return -1
	}
	for s[end-1] == ')' && ec > eo {
		ec--
		q := end - 1
		end = -1
		for q > first {
			r, n := utf8.DecodeLastRuneInString(s[first:q])
			if !bareExcluded(r) {
				end = q
				break
			}
			q -= n
		}
		if end < 0 {
			return -1
		}
	}
	return end
}

// bareExcluded reports whether r cannot end a bare link (besides whitespace
// and '<', which never occur inside one).
func bareExcluded(r rune) bool {
	switch r {
	case '.', ',', ':', ';', '"', '\'', ']':
		return true
	}
	return false
}
