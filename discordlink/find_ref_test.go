package discordlink

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// findRef is a naive reference for Find, written from the grammar
// (simple-markdown's rules with Discord's overrides) rather than from Find's
// code. It follows simple-markdown's parse loop literally: at each token
// boundary it tries the rules in simple-markdown's order (codeBlock 4,
// escape 12, autolink 14, Discord's mentions, url 16, link 17,
// em/strong/u 21 by quality, del 22, inlineCode 23, spoiler and text 25),
// takes the first match, recurses into paired rules' inner text, and
// otherwise lets the text rule consume up to the next boundary. Each rule is
// matched on the span substring, so '$' and lookaheads see the end of the
// span, as in simple-markdown's recursive parse(capture[1]).
//
// Rules without lookarounds are Go regexps: RE2's leftmost-first submatch
// semantics choose the same match as JavaScript's backtracking for them. A
// lookahead is rewritten as a consuming alternative that may also match the
// end of the span (\*\*(?!\*) becomes \*\*(?:[^*]|$)), and the match length
// is taken from the capture, not the whole match. JavaScript's \s is spelled
// out (jsWS), as is '.' (it excludes the four line terminators). The masked
// link's label, whose lookahead RE2 cannot express, is walked by hand; its
// href is a regexp tried after each label candidate, last to first, as the
// greedy star backtracks. Inline code's backreference becomes one regexp per
// opener length.
//
// maxDepth > 0 applies Find's nesting limit: inside a span nested maxDepth
// paired rules deep, the paired rules (spoiler, em, strong, u, del) are not
// tried, and every link found in that span is a spoiler if the span contains
// "||". maxDepth == 0 is uncapped (simple-markdown's own behaviour).
//
// Find's documented choices are applied: bare links use discord.py's regex
// with an unbalanced trailing ')' trimmed (U1), a masked link whose label
// contains http:// or https:// fails (L1), a masked link is MaskedSuppressed
// only when both '<' and '>' around its target were consumed (L2), and only
// http(s) targets are reported, though every autolink and masked link is
// consumed (L3). Links inside a masked link's label are never reported.
//
// It is O(n^2) or worse and meant for inputs of a few kilobytes.
func findRef(content string, maxDepth int) []Link {
	r := &refParser{s: content, maxDepth: maxDepth}
	r.parse(0, len(content), false, 0)
	return r.out
}

// jsWS is JavaScript's \s as a character-class body.
const jsWS = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

const (
	reS    = `[` + jsWS + `]`
	reAny  = `[\s\S]`
	reDot  = `[^\n\r\x{2028}\x{2029}]`
	reName = `[-_'\pL\pN\p{Devanagari}\p{Thai}]{1,32}`
)

var (
	// codeBlock (Discord's fence). /i applies only to the language class.
	refFence = regexp.MustCompile("^```(?:([a-zA-Z0-9_+\\-.#]+?)\\n)?\\n*([^\\n]" + reAny + "*?)\\n*```")
	// escape: ^\\([^0-9A-Za-z\s])
	refEscape = regexp.MustCompile(`^\\[^0-9A-Za-z` + jsWS + `]`)
	// autolink: ^<([^: >]+:\/[^ >]+)>
	refAutolink = regexp.MustCompile(`^<([^: >]+:/[^ >]+)>`)
	// Discord's ten message-formatting tokens (developer reference).
	refMentions = []*regexp.Regexp{
		regexp.MustCompile(`^<@!?\d+>`),
		regexp.MustCompile(`^<@&\d+>`),
		regexp.MustCompile(`^<#\d+>`),
		regexp.MustCompile(`^<@\$\d+>`),
		regexp.MustCompile(`^</` + reName + `(?: ` + reName + `){0,2}:\d+>`),
		regexp.MustCompile(`^<a?:\w+:\d+>`),
		regexp.MustCompile(`^<t:-?\d+(?::[tTdDfFsSR])?>`),
		regexp.MustCompile(`^<\d+:(?:customize|browse|guide|linked-roles(?::\d+)?)>`),
	}
	// url: discord.py's _URL_REGEX without steam://.
	refURL     = regexp.MustCompile(`^https?://[^` + jsWS + `<]+[^<.,:;"'\]` + jsWS + `]`)
	refURLFull = regexp.MustCompile(`^https?://[^` + jsWS + `<]+[^<.,:;"'\]` + jsWS + `]$`)
	// link, after "](": LINK_HREF_AND_TITLE then \). Groups: '<', target, '>'.
	refHref = regexp.MustCompile(`^` + reS + `*(<?)((?:\([^)]*\)|[^` + jsWS + `\\]|\\` + reDot + `)*?)(>?)(?:` + reS + `+['"]` + reAny + `*?['"])?` + reS + `*\)`)
	// strong: ^\*\*((?:\\[\s\S]|[^\\])+?)\*\*(?!\*)
	refStrong = regexp.MustCompile(`^\*\*((?:\\` + reAny + `|[^\\])+?)\*\*(?:[^*]|$)`)
	// em (*): ^\*(?=\S)((?:\*\*|\\[\s\S]|\s+(?:\\[\s\S]|[^\s\*\\]|\*\*)|[^\s\*\\])+?)\*(?!\*)
	// The (?=\S) is checked by hand.
	refEmStar = regexp.MustCompile(`^\*((?:\*\*|\\` + reAny + `|` + reS + `+(?:\\` + reAny + `|[^` + jsWS + `*\\]|\*\*)|[^` + jsWS + `*\\])+?)\*(?:[^*]|$)`)
	// em (_): ^\b_((?:__|\\[\s\S]|[^\\_])+?)_\b. The leading \b always holds
	// on the span substring.
	refEmUnder = regexp.MustCompile(`^_((?:__|\\` + reAny + `|[^\\_])+?)_\b`)
	// u: ^__((?:\\[\s\S]|[^\\])+?)__(?!_)
	refU = regexp.MustCompile(`^__((?:\\` + reAny + `|[^\\])+?)__(?:[^_]|$)`)
	// del (Discord's): ^~~([\s\S]+?)~~(?!_)
	refDel = regexp.MustCompile(`^~~(` + reAny + `+?)~~(?:[^_]|$)`)
	// spoiler (Discord's): ^\|\|([\s\S]+?)\|\|
	refSpoiler = regexp.MustCompile(`^\|\|(` + reAny + `+?)\|\|`)
	// text's \w+:\S lookahead.
	refWordColon = regexp.MustCompile(`^\w+:[^` + jsWS + `]`)
)

// refCode returns inline code's regexp for an opener of k backticks:
// ^(`+)([\s\S]*?[^`])\1(?!`) with \1 fixed to k backticks.
func refCode(k int) *regexp.Regexp {
	refCodeMu.Lock()
	defer refCodeMu.Unlock()
	if re, ok := refCodeRe[k]; ok {
		return re
	}
	q := "`{" + strconv.Itoa(k) + "}"
	re := regexp.MustCompile("^" + q + "(" + reAny + "*?[^`])" + q + "(?:[^`]|$)")
	refCodeRe[k] = re
	return re
}

var (
	refCodeMu sync.Mutex
	refCodeRe = map[int]*regexp.Regexp{}
)

type refParser struct {
	s        string
	maxDepth int
	out      []Link
}

// parse is simple-markdown's nestedParse over s[lo:hi].
func (r *refParser) parse(lo, hi int, spoiler bool, depth int) {
	capped := r.maxDepth > 0 && depth >= r.maxDepth
	if capped && strings.Contains(r.s[lo:hi], "||") {
		spoiler = true
	}
	for i := lo; i < hi; {
		i = r.token(i, hi, spoiler, depth, capped)
	}
}

// token parses one token at i and returns its end.
func (r *refParser) token(i, hi int, spoiler bool, depth int, capped bool) int {
	s := r.s
	src := s[i:hi]
	// codeBlock.
	if m := refFence.FindStringIndex(src); m != nil {
		return i + m[1]
	}
	// escape.
	if m := refEscape.FindStringIndex(src); m != nil {
		return i + m[1]
	}
	// autolink.
	if m := refAutolink.FindStringSubmatchIndex(src); m != nil {
		r.report(i+m[2], i+m[3], Suppressed, spoiler, "")
		return i + m[1]
	}
	// Discord's mentions.
	for _, re := range refMentions {
		if m := re.FindStringIndex(src); m != nil {
			return i + m[1]
		}
	}
	// url.
	if m := refURL.FindStringIndex(src); m != nil {
		if u, ok := refTrimParens(src[:m[1]]); ok {
			r.report(i, i+len(u), Bare, spoiler, "")
			return i + len(u)
		}
	}
	// link.
	if end, ok := r.masked(i, hi, spoiler); ok {
		return end
	}
	if !capped {
		// em, strong, u: the best quality wins (em len+0.2, strong len+0.1,
		// u len; a later rule replaces only a strictly better one).
		type cand struct{ q, lo, hi, end int }
		var best *cand
		try := func(re *regexp.Regexp, open, bonus int) {
			if m := re.FindStringSubmatchIndex(src); m != nil {
				end := m[3] + open
				c := cand{q: 10*end + bonus, lo: i + m[2], hi: i + m[3], end: i + end}
				if best == nil || c.q > best.q {
					best = &c
				}
			}
		}
		if len(src) > 1 && src[0] == '*' {
			if c, _ := utf8.DecodeRuneInString(src[1:]); !isJSSpace(c) {
				try(refEmStar, 1, 2)
			}
		}
		try(refEmUnder, 1, 2)
		try(refStrong, 2, 1)
		try(refU, 2, 0)
		if best != nil {
			r.parse(best.lo, best.hi, spoiler, depth+1)
			return best.end
		}
		// del.
		if m := refDel.FindStringSubmatchIndex(src); m != nil {
			r.parse(i+m[2], i+m[3], spoiler, depth+1)
			return i + m[3] + 2
		}
	}
	// inlineCode.
	if k := len(src) - len(strings.TrimLeft(src, "`")); k > 0 {
		for ; k > 0; k-- {
			if m := refCode(k).FindStringSubmatchIndex(src); m != nil {
				return i + m[3] + k
			}
		}
	}
	// spoiler.
	if !capped {
		if m := refSpoiler.FindStringSubmatchIndex(src); m != nil {
			r.parse(i+m[2], i+m[3], true, depth+1)
			return i + m[3] + 2
		}
	}
	// text: ^[\s\S]+?(?=[^0-9A-Za-z\s\u00c0-\uffff]|\n\n|\n|\w+:\S|$)
	_, n := utf8.DecodeRuneInString(src)
	p := i + n
	for p < hi && !refTextBoundary(s[p:hi]) {
		_, n := utf8.DecodeRuneInString(s[p:hi])
		p += n
	}
	return p
}

// refTextBoundary reports whether the text rule's lookahead holds at the
// start of rest.
func refTextBoundary(rest string) bool {
	c, _ := utf8.DecodeRuneInString(rest)
	isAlnum := '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
	if !isAlnum && !isJSSpace(c) && !(0xC0 <= c && c <= 0xFFFF) && c <= 0xFFFF {
		return true
	}
	return c == '\n' || refWordColon.MatchString(rest)
}

// refTrimParens applies U1 to a bare-link match u: while u ends in ')' and
// has more ')' than '(', drop the ')' and re-apply the last-character rule.
// ok is false if what is left no longer matches the URL regexp.
func refTrimParens(u string) (string, bool) {
	for strings.HasSuffix(u, ")") && strings.Count(u, ")") > strings.Count(u, "(") {
		u = u[:len(u)-1]
		for u != "" {
			c, n := utf8.DecodeLastRuneInString(u)
			if !strings.ContainsRune(`<.,:;"']`, c) && !isJSSpace(c) {
				break
			}
			u = u[:len(u)-n]
		}
	}
	return u, refURLFull.MatchString(u)
}

// masked tries simple-markdown's link rule,
// ^\[(LINK_INSIDE)\]\(LINK_HREF_AND_TITLE\) with
// LINK_INSIDE = (?:\[[^\]]*\]|[^\[\]]|\](?=[^\[]*\]))*, at i.
func (r *refParser) masked(i, hi int, spoiler bool) (int, bool) {
	s := r.s
	if s[i] != '[' {
		return 0, false
	}
	// The star's iterations are deterministic: at each position at most one
	// alternative applies. stops lists every position the star can stop at.
	stops := []int{i + 1}
	for p := i + 1; p < hi; {
		switch s[p] {
		case '[':
			k := strings.IndexByte(s[p+1:hi], ']')
			if k < 0 {
				goto walked
			}
			p += 1 + k + 1
		case ']':
			k := strings.IndexByte(s[p+1:hi], ']')
			if k < 0 || strings.IndexByte(s[p+1:p+1+k], '[') >= 0 {
				goto walked
			}
			p++
		default:
			_, n := utf8.DecodeRuneInString(s[p:hi])
			p += n
		}
		stops = append(stops, p)
	}
walked:
	// The star is greedy: the longest label is tried first.
	for k := len(stops) - 1; k >= 0; k-- {
		q := stops[k]
		if q+1 >= hi || s[q] != ']' || s[q+1] != '(' {
			continue
		}
		m := refHref.FindStringSubmatchIndex(s[q+2 : hi])
		if m == nil {
			continue
		}
		label := s[i+1 : q]
		if strings.Contains(label, "http://") || strings.Contains(label, "https://") {
			return 0, false // L1: the rule fails
		}
		kind := Masked
		if m[3] > m[2] && m[7] > m[6] {
			kind = MaskedSuppressed
		}
		r.report(q+2+m[4], q+2+m[5], kind, spoiler, label)
		return q + 2 + m[1], true
	}
	return 0, false
}

// report appends s[start:end] if it is an http(s) link.
func (r *refParser) report(start, end int, k Kind, spoiler bool, label string) {
	u := r.s[start:end]
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return
	}
	r.out = append(r.out, Link{URL: u, Start: start, End: end, Kind: k, Spoiler: spoiler, Label: label})
}

// TestFindRefNestingLimit pins the reference's nesting-limit rule with small
// limits: inside a span at the limit, paired rules are not tried, and its
// links are spoilers if the span contains "||".
func TestFindRefNestingLimit(t *testing.T) {
	for _, c := range []struct {
		in    string
		depth int
		want  string
	}{
		{"||https://a.com/x||", 0, `Bare "https://a.com/x" spoiler`},
		{"||https://a.com/x||", 1, `Bare "https://a.com/x" spoiler`},
		{"**||https://a.com/x||**", 0, `Bare "https://a.com/x" spoiler`},
		{"**||https://a.com/x||**", 2, `Bare "https://a.com/x" spoiler`},
		{"**||https://a.com/x||**", 1, `Bare "https://a.com/x||" spoiler`},
		{"**a ~~https://a.com/x~~ b**", 1, `Bare "https://a.com/x~~"`},
		{"**a ~~https://a.com/x~~ b**", 2, `Bare "https://a.com/x"`},
		{"**a <https://a.com/x> [l](https://b.com/y) `https://c.com/z`**", 1, `Suppressed "https://a.com/x"` + "\n" + `Masked "https://b.com/y" label="l"`},
		{"__||a||https://a.com/x__", 1, `Bare "https://a.com/x" spoiler`},
	} {
		want := c.want + "\n"
		if got := formatLinks(findRef(c.in, c.depth)); got != want {
			t.Errorf("findRef(%q, %d):\n%swant:\n%s", c.in, c.depth, got, want)
		}
	}
}
