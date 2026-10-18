package discordlink

// The rules Find emulates. Each comment quotes the JavaScript regular
// expression of the rule, as Discord's client evaluates it on the span
// substring s[i:hi] (so $ and lookaheads see the span's end). The regex text
// is from simple-markdown (MIT, copyright Khan Academy and Aria Buckles),
// with Discord's overrides for del and spoilers; the matchers are
// hand-written and follow JavaScript's backtracking order exactly.

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// runeLen is the length of the rune at p (an invalid byte is a 1-byte rune).
func runeLen(s string, p, hi int) int {
	if s[p] < utf8.RuneSelf {
		return 1
	}
	_, n := utf8.DecodeRuneInString(s[p:hi])
	return n
}

func isAlnum(b byte) bool {
	return '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// isWord is JavaScript's \w (ASCII only, without the u flag).
func isWord(b byte) bool { return isAlnum(b) || b == '_' }

func isQuote(b byte) bool { return b == '"' || b == '\'' }

func hasHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// escape: ^\\([^0-9A-Za-z\s]). A backslash before anything but an ASCII
// letter or digit or JS whitespace consumes it, so \https:// is still a link.
// JavaScript consumes one UTF-16 unit and Go the whole rune; no rule starts
// with a low surrogate, so the two agree.
func (sc *scanner) escape(i, hi int) int {
	s := sc.x.s
	if i+1 < hi {
		r, n := utf8.DecodeRuneInString(s[i+1 : hi])
		if !(r < utf8.RuneSelf && isAlnum(byte(r))) && !isJSSpace(r) {
			return i + 1 + n
		}
	}
	return i + 1
}

// code tries a fence, then inline code, at a '`'. Neither is a link nor
// recursed into. It returns where the scan resumes.
func (sc *scanner) code(i, hi int) int {
	x := sc.x
	s := x.s
	if i+2 < hi && s[i+1] == '`' && s[i+2] == '`' {
		if e := sc.fence(i, hi); e >= 0 {
			return e
		}
	}
	// Inline code: ^(`+)([\s\S]*?[^`])\1(?!`). The opener run is greedy and
	// backtracks, k = K…1; for each k the lazy content ends at the first raw
	// backtick run of exactly k bytes after the opener run (backslashes do
	// not split raw runs). Spans end on a delimiter that is not '`', so no
	// run is cut by hi.
	e := i + 1
	for e < hi && s[e] == '`' {
		e++
	}
	x.steps += e - i
	for k := e - i; k >= 1 && !x.over(); k-- {
		x.steps++
		if r := x.tickRun(k, e+1); r >= 0 && r+k <= hi {
			return r + k
		}
	}
	// Later starts inside the run have a smaller K and the same candidates,
	// and a fence fails there too.
	return e
}

// fence: ^```(?:([a-z0-9_+\-.#]+?)\n)?\n*([^\n][^]*?)\n*```/i, at i with
// s[i:i+3] == "```". It returns the end of the match, or -1. The content
// starts at the first non-'\n' after the opener (or after the language and
// its '\n'); the closer is the first "```" at least one byte after that.
// The language variant is preferred; the earlier start is queried first so
// the cursor stays monotone.
func (sc *scanner) fence(i, hi int) int {
	x := sc.x
	s := x.s
	skipNL := func(p int) int {
		for p < hi && s[p] == '\n' {
			p++
			x.steps++
		}
		return p
	}
	plain := skipNL(i + 3)
	lang := -1
	j := i + 3
	for j < hi && isLangByte(s[j]) {
		j++
	}
	x.steps += j - i - 3
	if j > i+3 && j < hi && s[j] == '\n' {
		if p := skipNL(j + 1); p < hi {
			lang = p
		}
	}
	end := -1
	if plain < hi {
		if t := x.nextSub(&x.fenceC, plain+1, "```", nil); t+3 <= hi {
			end = t + 3
		}
	}
	if lang >= 0 {
		if t := x.nextSub(&x.fenceC, lang+1, "```", nil); t+3 <= hi {
			end = t + 3
		}
	}
	if x.over() {
		return -1
	}
	return end
}

// isLangByte is [a-z0-9_+\-.#] under the i flag (ASCII only: JavaScript's
// non-unicode case folding never maps a non-ASCII letter into it).
func isLangByte(b byte) bool {
	return isAlnum(b) || b == '_' || b == '+' || b == '-' || b == '.' || b == '#'
}

// angle tries an autolink, then the mention forms, at a '<'. It returns
// where the scan resumes.
func (sc *scanner) angle(i, hi int, spoiler bool) int {
	if e := sc.autolink(i, hi, spoiler); e >= 0 {
		return e
	}
	if e := sc.mention(i, hi); e >= 0 {
		return e
	}
	return i + 1
}

// autolink: ^<([^: >]+:\/[^ >]+)>. Both classes are greedy and stop at the
// only byte that can follow them, so the match is deterministic. A match is
// consumed whatever its scheme and reported, as Suppressed, only for an
// http(s) target. The target may hold '\n', tabs, '<' or U+00A0.
func (sc *scanner) autolink(i, hi int, spoiler bool) int {
	x := sc.x
	s := x.s
	p1 := x.next(&x.autoC, i+1, func(p int) bool { b := s[p]; return b == ':' || b == ' ' || b == '>' })
	if x.over() || p1 >= hi || p1 == i+1 || s[p1] != ':' || p1+1 >= hi || s[p1+1] != '/' {
		return -1
	}
	p2 := x.next(&x.autoEndC, p1+2, func(p int) bool { b := s[p]; return b == ' ' || b == '>' })
	if x.over() || p2 >= hi || p2 < p1+3 || s[p2] != '>' {
		return -1
	}
	if hasHTTP(s[i+1 : p2]) {
		sc.emit(Link{URL: s[i+1 : p2], Start: i + 1, End: p2, Kind: Suppressed, Spoiler: spoiler})
	}
	return p2 + 1
}

// mention matches the ten documented message-formatting forms at a '<' and
// returns the end of the match, or -1:
//
//	<@id> <@!id> <@&id> <@$id> <#id>      id: [0-9]+
//	<:name:id> <a:name:id>                name: [A-Za-z0-9_]+
//	<t:ts> <t:ts:S>                       ts: -?[0-9]+, S: one of tTdDfFsSR
//	</name:id>, with up to two more " name" parts
//	                                      name: 1–32 of [-_'A-Za-z0-9], letters, numbers, Devanagari or Thai
//	<id:customize> <id:browse> <id:guide> <id:linked-roles> <id:linked-roles:id>
//
// None is a link, but a mention consumes its bytes, so a '_' inside one
// pairs with nothing. No class contains '<', so mention scans never overlap.
func (sc *scanner) mention(i, hi int) int {
	x := sc.x
	s := x.s
	j := i + 1
	if j+1 >= hi {
		return -1
	}
	switch s[j] {
	case '@':
		j++
		if b := s[j]; b == '!' || b == '&' || b == '$' {
			j++
		}
		j = sc.digits(j, hi)
	case '#':
		j = sc.digits(j+1, hi)
	case ':':
		j = sc.emoji(j+1, hi)
	case 'a', 't':
		if s[j+1] != ':' {
			return -1
		}
		if s[j] == 'a' {
			j = sc.emoji(j+2, hi)
			break
		}
		j += 2
		if j < hi && s[j] == '-' {
			j++
		}
		if j = sc.digits(j, hi); j >= 0 && j+1 < hi && s[j] == ':' && strings.IndexByte("tTdDfFsSR", s[j+1]) >= 0 {
			j += 2
		}
	case 'i':
		if !strings.HasPrefix(s[j:hi], "id:") {
			return -1
		}
		k := j + 3
		j = -1
		for _, t := range [...]string{"customize", "browse", "guide", "linked-roles"} {
			if strings.HasPrefix(s[k:hi], t) {
				j = k + len(t)
				break
			}
		}
		if j >= 0 && s[k] == 'l' && j < hi && s[j] == ':' {
			j = sc.digits(j+1, hi)
		}
	case '/':
		j = sc.slash(j+1, hi)
	default:
		return -1
	}
	if j < 0 || j >= hi || s[j] != '>' {
		return -1
	}
	return j + 1
}

// digits returns the end of a run of one or more ASCII digits at j, or -1.
func (sc *scanner) digits(j, hi int) int {
	s := sc.x.s
	k := j
	for k < hi && '0' <= s[k] && s[k] <= '9' {
		k++
	}
	sc.x.steps += k - j
	if k == j {
		return -1
	}
	return k
}

// emoji matches name:id> up to the '>' and returns its position, or -1.
func (sc *scanner) emoji(j, hi int) int {
	s := sc.x.s
	k := j
	for k < hi && isWord(s[k]) {
		k++
	}
	sc.x.steps += k - j
	if k == j || k >= hi || s[k] != ':' {
		return -1
	}
	return sc.digits(k+1, hi)
}

// slash matches a slash command's name parts and :id, up to the '>'.
// Discord's names are [-_'\p{L}\p{N}\p{sc=Deva}\p{sc=Thai}]{1,32}.
func (sc *scanner) slash(j, hi int) int {
	x := sc.x
	s := x.s
	for part := 0; ; part++ {
		k := 0
		for j < hi && k < 32 {
			b := s[j]
			n := 1
			if b >= utf8.RuneSelf {
				var r rune
				r, n = utf8.DecodeRuneInString(s[j:hi])
				if !isNameRune(r) {
					break
				}
			} else if !isWord(b) && b != '-' && b != '\'' {
				break
			}
			j += n
			k++
			x.steps++
		}
		if k == 0 {
			return -1
		}
		if part < 2 && j < hi && s[j] == ' ' {
			j++
			continue
		}
		break
	}
	if j >= hi || s[j] != ':' {
		return -1
	}
	return sc.digits(j+1, hi)
}

// isNameRune reports whether the non-ASCII rune r may appear in a slash
// command name: a letter, a number, or a Devanagari or Thai code point. An
// invalid byte (utf8.RuneError) is none of these.
func isNameRune(r rune) bool {
	return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Devanagari, r) || unicode.Is(unicode.Thai, r))
}

// pairClose finds the closer of a doubled-delimiter rule at i, where
// s[i] == d, and returns its position, or -1. The rules are
//
//	strong   ^\*\*((?:\\[\s\S]|[^\\])+?)\*\*(?!\*)
//	u        ^__((?:\\[\s\S]|[^\\])+?)__(?!_)
//	del      ^~~([\s\S]+?)~~(?!_)                 (Discord's)
//	spoiler  ^\|\|([\s\S]+?)\|\|                  (Discord's)
//
// so the closer is the first dd at p ≥ i+3, unescaped if esc (backslash
// parity is global: no span starts after a '\'), and not followed by look
// (0: no lookahead) unless it ends the span.
func (sc *scanner) pairClose(i, hi int, c *nextCache, dd string, esc bool, look byte) int {
	x := sc.x
	s := x.s
	if i+5 > hi || s[i+1] != dd[0] {
		return -1
	}
	if esc {
		x.buildRuns() // the escape bitset
	}
	n := len(s)
	g := x.nextSub(c, i+3, dd, func(p int) bool {
		return p+1 < n && s[p+1] == dd[0] &&
			(look == 0 || p+2 == n || s[p+2] != look) && (!esc || !x.escaped(p))
	})
	if x.over() {
		return -1
	}
	if g+2 <= hi {
		return g
	}
	// The global answer is past the span. The span's end satisfies the
	// lookahead, so a pair ending exactly at hi can still close.
	if p := hi - 2; s[p] == dd[0] && s[p+1] == dd[0] && (!esc || !x.escaped(p)) {
		return p
	}
	return -1
}

// emStar finds the closer of
//
//	em  ^\*(?=\S)((?:\*\*|\\[\s\S]|\s+(?:\\[\s\S]|[^\s\*\\]|\*\*)|[^\s\*\\])+?)\*(?!\*)
//
// at i (s[i] == '*') and returns its position, or -1. The content's atoms are
// forced: an unescaped '*' can only be half of a "**" atom, so an even run is
// passed and the last '*' of an odd run is a stop. That stop closes the em
// unless the run is a single '*' directly after unescaped whitespace, which
// no atom can consume, and which blocks the em instead. The runs that the
// span clips (the one at i+1, which shares the opener, and the one at hi-1)
// are evaluated directly; every other run is classified globally.
func (sc *scanner) emStar(i, hi int) int {
	x := sc.x
	s := x.s
	if i+1 >= hi {
		return -1
	}
	if r, _ := utf8.DecodeRuneInString(s[i+1 : hi]); isJSSpace(r) {
		return -1
	}
	x.buildRuns()
	from := i + 2
	if s[i+1] == '*' {
		e := min(x.runEnd(i+1), hi)
		if l := e - (i + 1); l%2 == 1 {
			if l >= 3 {
				return e - 1
			}
			return -1
		}
		from = e
	}
	if from >= hi {
		return -1
	}
	g := x.nextSub(&x.emStarC, from, "*", x.oddRunEnd)
	if x.over() {
		return -1
	}
	if g < hi-1 {
		if x.runStart(g) == g && sc.wsBefore(g, i+1) {
			return -1
		}
		return g
	}
	if !x.inRun(hi-1) || s[hi-1] != '*' {
		return -1
	}
	st := max(x.runStart(hi-1), from)
	if l := hi - st; l%2 == 0 || l == 1 && sc.wsBefore(st, i+1) {
		return -1
	}
	return hi - 1
}

// wsBefore reports whether the rune before p (and at or after lo) is
// unescaped JS whitespace.
func (sc *scanner) wsBefore(p, lo int) bool {
	if p <= lo {
		return false
	}
	r, n := utf8.DecodeLastRuneInString(sc.x.s[lo:p])
	return isJSSpace(r) && !sc.x.escaped(p-n)
}

// emUnder finds the closer of
//
//	em  ^\b_((?:__|\\[\s\S]|[^\\_])+?)_\b
//
// at i (s[i] == '_') and returns its position, or -1. ^\b always holds on
// the span substring. As for emStar, even runs are passed and the last '_' of
// an odd run is a stop: it closes the em unless a word byte follows it,
// which blocks the em.
func (sc *scanner) emUnder(i, hi int) int {
	x := sc.x
	s := x.s
	if i+1 >= hi {
		return -1
	}
	x.buildRuns()
	from := i + 2
	if s[i+1] == '_' {
		e := min(x.runEnd(i+1), hi)
		if l := e - (i + 1); l%2 == 1 {
			if l >= 3 && (e == hi || !isWord(s[e])) {
				return e - 1
			}
			return -1
		}
		from = e
	}
	if from >= hi {
		return -1
	}
	g := x.nextSub(&x.emUnderC, from, "_", x.oddRunEnd)
	if x.over() {
		return -1
	}
	if g < hi-1 {
		if isWord(s[g+1]) {
			return -1
		}
		return g
	}
	if !x.inRun(hi-1) || s[hi-1] != '_' {
		return -1
	}
	if (hi-max(x.runStart(hi-1), from))%2 == 1 {
		return hi - 1
	}
	return -1
}

// paired tries the paired rules at i (s[i] is '|', '*', '_' or '~') and
// returns the inner span and the end of the match, or end < 0. '*' and '_'
// compute both of their rules and take the longer match, em on a tie, as
// simple-markdown's quality does (em length+0.2, strong +0.1, u +0).
func (sc *scanner) paired(i, hi int) (lo2, hi2, end int) {
	x := sc.x
	switch x.s[i] {
	case '|':
		if p := sc.pairClose(i, hi, &x.spoilerC, "||", false, 0); p >= 0 {
			return i + 2, p, p + 2
		}
	case '~':
		if p := sc.pairClose(i, hi, &x.delC, "~~", false, '_'); p >= 0 {
			return i + 2, p, p + 2
		}
	case '*':
		ps := sc.pairClose(i, hi, &x.strongC, "**", true, '*')
		pe := sc.emStar(i, hi)
		return pick(i, ps, pe)
	case '_':
		pu := sc.pairClose(i, hi, &x.uC, "__", true, '_')
		pe := sc.emUnder(i, hi)
		return pick(i, pu, pe)
	}
	return 0, 0, -1
}

// pick chooses between a doubled closer pd and an em closer pe at i.
func pick(i, pd, pe int) (lo2, hi2, end int) {
	if pe >= 0 && (pd < 0 || pe+1 >= pd+2) {
		return i + 1, pe, pe + 1
	}
	if pd >= 0 {
		return i + 2, pd, pd + 2
	}
	return 0, 0, -1
}

// masked tries a masked link at i (s[i] == '['):
//
//	^\[(LINK_INSIDE)\]\(LINK_HREF_AND_TITLE\)
//	LINK_INSIDE          (?:\[[^\]]*\]|[^\[\]]|\](?=[^\[]*\]))*
//	LINK_HREF_AND_TITLE  \s*<?((?:\([^)]*\)|[^\s\\]|\\.)*?)>?(?:\s+['"]([\s\S]*?)['"])?\s*
//
// It returns where the scan resumes. A label that contains "http://" or
// "https://" fails the rule, so the scan goes on inside it. A match is
// consumed; it is reported only for an http(s) target, raw (a "\)" stays),
// as MaskedSuppressed iff both the '<' and the '>' were consumed.
func (sc *scanner) masked(i, hi int, spoiler bool) int {
	x := sc.x
	s := x.s
	if !x.masked {
		return i + 1
	}
	x.buildMasked()
	c := sc.label(i+1, hi)
	if c < 0 {
		return i + 1
	}
	if p := x.nextSub(&x.httpC, i+1, "http", func(p int) bool { return hasHTTP(s[p:]) }); p < c {
		l := len("http://")
		if s[p+4] == 's' {
			l++
		}
		if p+l <= c {
			return i + 1
		}
	}
	h, ok := sc.href(c+2, hi)
	if !ok || x.over() {
		return i + 1
	}
	if hasHTTP(s[h.t:h.q]) {
		sc.emit(Link{URL: s[h.t:h.q], Start: h.t, End: h.q, Kind: h.kind, Spoiler: spoiler, Label: s[i+1 : c]})
	}
	return h.end
}

// label walks LINK_INSIDE from p0 and returns the label end the greedy star
// settles on: the last "](" candidate on the walk whose href matches, or -1.
// The walk is deterministic: '[' jumps past the next ']' (a barrier without
// one), a ']' continues only if another ']' comes before the next '[' (it is
// a candidate if '(' follows, either way), anything else advances one rune
// (so the walk jumps to the next bracket). Walks from different openers merge
// only right after a ']', so the result is memoized there per span and every
// bracket is walked O(1) times per span. Candidates are evaluated in order;
// the href memos make that linear.
func (sc *scanner) label(p0, hi int) int {
	x := sc.x
	s := x.s
	tag := sc.span
	last, stop := -1, -1
	p := p0
walk:
	for p < hi {
		if x.over() {
			return -1
		}
		x.steps++
		switch s[p] {
		case '[':
			q := int(x.nClose[p+1])
			if q >= hi {
				break walk
			}
			p = q + 1
		case ']':
			if p+1 < hi && s[p+1] == '(' && sc.hrefOK(p+2, hi) {
				last = p
			}
			if q := int(x.nBracket[p+1]); q >= hi || s[q] != ']' {
				break walk
			}
			p++
		default:
			// Bytes other than brackets advance one rune each and cannot
			// merge with another walk: jump to the next bracket.
			p = min(int(x.nBracket[p]), hi)
			continue
		}
		if x.labTag[p] == tag {
			stop = int(x.labVal[p])
			break
		}
	}
	best := last
	if stop >= 0 {
		best = stop
	}
	// Memoize every position after a ']' that this walk reached.
	for p := p0; p < hi; {
		x.steps++
		switch s[p] {
		case '[':
			q := int(x.nClose[p+1])
			if q >= hi {
				return best
			}
			p = q + 1
		case ']':
			if q := int(x.nBracket[p+1]); q >= hi || s[q] != ']' {
				return best
			}
			p++
		default:
			p = min(int(x.nBracket[p]), hi)
			continue
		}
		if x.labTag[p] == tag {
			return best
		}
		v := best
		if stop < 0 && last < p {
			v = -1
		}
		x.labTag[p], x.labVal[p] = tag, int32(v)
	}
	return best
}

// hrefMatch is a matched LINK_HREF_AND_TITLE plus its ')': the target
// s[t:q], the end of the match and the link kind.
type hrefMatch struct {
	t, q, end int
	kind      Kind
}

func (sc *scanner) hrefOK(p, hi int) bool {
	_, ok := sc.href(p, hi)
	return ok
}

// href matches LINK_HREF_AND_TITLE and the closing ')' from p, in JavaScript
// backtracking order: the leading \s* in full, then the target after '<'
// (when there is one), then the target without it, and last the one path
// that backtracking \s* adds, an empty target followed by a title.
func (sc *scanner) href(p, hi int) (hrefMatch, bool) {
	x := sc.x
	s := x.s
	b := x.wsEnd(p, hi)
	if b < hi && s[b] == '<' {
		if q := sc.target(b+1, hi); q >= 0 {
			end, gt, _ := sc.suffix(q, hi)
			k := Masked
			if gt {
				k = MaskedSuppressed
			}
			return hrefMatch{b + 1, q, end, k}, true
		}
	}
	if q := sc.target(b, hi); q >= 0 {
		end, _, _ := sc.suffix(q, hi)
		return hrefMatch{b, q, end, Masked}, true
	}
	if b > p && b < hi && isQuote(s[b]) {
		if t := int(x.nTitle[b+1]); t < hi {
			return hrefMatch{b, b, t + 1, Masked}, true
		}
	}
	return hrefMatch{}, false
}

// suffix matches >?(?:\s+['"]([\s\S]*?)['"])?\s*\) at q and returns the
// end of the match and whether a '>' was consumed. The lazy title ends at
// the first quote followed by whitespace and ')' (nTitle). With '>' at q,
// the variant without it can never match, so it is not tried.
func (sc *scanner) suffix(q, hi int) (end int, gt, ok bool) {
	x := sc.x
	s := x.s
	if q < hi && s[q] == '>' {
		q, gt = q+1, true
	}
	r := x.wsEnd(q, hi)
	if r > q && r < hi && isQuote(s[r]) {
		if t := int(x.nTitle[r+1]); t < hi {
			return t + 1, gt, true
		}
	}
	if r < hi && s[r] == ')' {
		return r + 1, gt, true
	}
	return 0, false, false
}

// target runs the lazy target (?:\([^)]*\)|[^\s\\]|\\.)*? from t as a
// depth-first search in JavaScript's order: at each position the suffix,
// then a parenthesized group, then one non-space non-'\' rune, then '\' and
// a rune that is not a line terminator. It returns the position where the
// suffix first matches, or -1. A position's outcome depends only on the
// position and the span end, so it is memoized per span, success and
// failure alike; the stack is explicit.
func (sc *scanner) target(t, hi int) int {
	x := sc.x
	if v, ok := sc.hrefMemo(t); ok {
		return v
	}
	if _, _, ok := sc.suffix(t, hi); ok {
		return sc.succeed(nil, t)
	}
	st := x.stack[:1]
	st[0] = int32(t)
	last := -1 // the child of the top frame that just failed; -1: none yet
	for len(st) > 0 {
		if x.over() {
			return -1
		}
		x.steps++
		p := int(st[len(st)-1])
		c := sc.child(p, last, hi)
		if c < 0 {
			x.hrefTag[p], x.hrefVal[p] = sc.span, -1
			st = st[:len(st)-1]
			last = p
			continue
		}
		last = c
		if v, ok := sc.hrefMemo(c); ok {
			if v >= 0 {
				return sc.succeed(st, v)
			}
			continue
		}
		if _, _, ok := sc.suffix(c, hi); ok {
			return sc.succeed(append(st, int32(c)), c)
		}
		st = append(st, int32(c))
		last = -1
	}
	return -1
}

func (sc *scanner) hrefMemo(p int) (int, bool) {
	if sc.x.hrefTag[p] != sc.span {
		return 0, false
	}
	return int(sc.x.hrefVal[p]), true
}

// succeed memoizes v for every frame on the stack, and for its last frame
// (or the search's start when the stack is nil), and returns v.
func (sc *scanner) succeed(st []int32, v int) int {
	x := sc.x
	if st == nil {
		x.hrefTag[v], x.hrefVal[v] = sc.span, int32(v)
	}
	for _, p := range st {
		x.hrefTag[p], x.hrefVal[p] = sc.span, int32(v)
	}
	x.steps += len(st)
	return v
}

// child returns the DFS child of p to try after prev (-1: the first), or
// -1 when p has no more. A '(' has two: past its group, then itself as a
// plain rune. The children are distinct, so prev identifies the branch.
func (sc *scanner) child(p, prev, hi int) int {
	x := sc.x
	s := x.s
	if p >= hi {
		return -1
	}
	switch s[p] {
	case '(':
		g := int(x.nParen[p+1]) + 1
		switch {
		case prev < 0 && g <= hi:
			return g
		case prev < 0, prev == g:
			return p + 1
		}
		return -1
	case '\\':
		if prev >= 0 || p+1 >= hi {
			return -1
		}
		r, n := utf8.DecodeRuneInString(s[p+1 : hi])
		if isLineTerm(r) {
			return -1
		}
		return p + 1 + n
	}
	if prev >= 0 {
		return -1
	}
	r, n := utf8.DecodeRuneInString(s[p:hi])
	if isJSSpace(r) {
		return -1
	}
	return p + n
}
