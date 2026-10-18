package discordlink

import "strings"

// maxDepth is the nesting limit for paired rules (spoiler, strong, em, u,
// del); see the package doc's Nesting limit.
const maxDepth = 16

// Find returns http(s) links in ascending, non-overlapping order. It tokenizes in
// Discord precedence:
//
//	backslash escapes, ``` fences, `inline` code (skipped), <scheme:/…>,
//	[label](url|<url>), ||spoiler|| (recursed), ** __ ~~ * _ (recursed),
//	bare links via (?:https?)://[^\s<]+[^<.,:;"'\]\s] with the JS-Unicode \s set
//	(discord.py's _URL_REGEX minus steam://), then <@id> <@!id> <@&id> <#id>
//	<@$id> </name sub:id> <:n:id> <a:n:id> <t:ts[:S]> <id:TYPE> as non-links.
//
// '*' and '_' openers take the longer of the two emphasis matches (strong or
// em, u or em; em on a tie), as simple-markdown does. A masked link whose
// label contains "http://" or "https://" is not a masked link: the scan goes
// on inside it and finds those bare links instead. An autolink or masked link
// whose target is not http(s) is consumed and not reported. Paired rules nest
// at most 16 deep (see Nesting limit). Schemeless text is never a link. Find
// never panics, on any input, and runs in O(len(content) × nesting depth):
// closers are found through next-occurrence tables, never by rescanning.
// Those tables take up to 48 bytes of memory per byte of content (about 40
// when it holds every kind of opener), allocated at once, so a caller that
// accepts content far longer than a Discord message should bound its length
// before calling Find.
// Content longer than 2^31-1 bytes (longer than 214,748,364 bytes on 32-bit
// platforms) is not scanned and yields no links.
func Find(content string) []Link {
	links, _, _, _ := findSteps(content, 0)
	return links
}

// findSteps is Find with its work counters. budget > 0 stops the scan once
// more than budget steps were taken (aborted is then true and links is
// meaningless); steps counts every loop iteration and rescans every
// non-monotone cursor query. Find == findSteps(content, 0).
func findSteps(content string, budget int) (links []Link, steps, rescans int, aborted bool) {
	// Every link contains "http://" or "https://".
	nh := strings.Count(content, "http")
	if nh == 0 || len(content) > maxContent {
		return nil, 0, 0, false
	}
	x := &index{s: content, budget: budget, nhttp: nh}
	x.prepare()
	sc := scanner{x: x}
	sc.scan(0, len(content), false, 0)
	return sc.out, x.steps, x.rescans, x.over()
}

// scanner walks spans of x.s and appends the links it finds to out. span
// identifies the span being scanned, for span-tagged memos.
type scanner struct {
	x    *index
	out  []Link
	span int32
}

// ruleStart marks the bytes that can start a rule Find models. The text
// rule stops before every ASCII punctuation byte and before every \w+:\S, so
// each of these is a token boundary wherever the scan meets it, and trying
// the rules at every byte is exact.
var ruleStart = [256]bool{'\\': true, '`': true, '<': true, '[': true, '|': true, '*': true, '_': true, '~': true, 'h': true}

// scan scans the span [lo, hi) at nesting depth depth; spoiler reports
// whether an enclosing span is a spoiler. A parent skips the spans it
// recurses into, so every byte is scanned by exactly one span loop.
func (sc *scanner) scan(lo, hi int, spoiler bool, depth int) {
	x := sc.x
	s := x.s
	x.spans++
	sc.span = x.spans
	span := sc.span
	capped := depth >= maxDepth
	if capped && !spoiler {
		// At the cap, paired rules are not tried. A link in a capped span
		// that contains "||" might be inside a spoiler Discord would
		// parse, so it is reported as spoilered. Capped spans are
		// disjoint, so this costs O(n) in total.
		x.steps += hi - lo
		spoiler = strings.Contains(s[lo:hi], "||")
	}
	for i := lo; i < hi; {
		if x.over() {
			return
		}
		x.steps++
		b := s[i]
		if !ruleStart[b] {
			i++
			for i < hi && !ruleStart[s[i]] {
				i++
				x.steps++
			}
			continue
		}
		switch b {
		case '\\':
			i = sc.escape(i, hi)
		case '`':
			i = sc.code(i, hi)
		case '<':
			i = sc.angle(i, hi, spoiler)
		case '[':
			i = sc.masked(i, hi, spoiler)
		case 'h':
			e := bareEnd(s, i, hi)
			if e < 0 {
				i++
				continue
			}
			x.steps += e - i
			sc.emit(Link{URL: s[i:e], Start: i, End: e, Kind: Bare, Spoiler: spoiler})
			i = e
		default: // '|', '*', '_', '~'
			if capped {
				i++
				continue
			}
			lo2, hi2, end := sc.paired(i, hi)
			if end < 0 {
				i++
				continue
			}
			sc.scan(lo2, hi2, spoiler || b == '|', depth+1)
			sc.span = span
			i = end
		}
	}
}

// emit appends l to the output. The first append sizes the slice by the
// number of "http" occurrences, capped, so a typical message allocates once.
func (sc *scanner) emit(l Link) {
	if sc.out == nil {
		sc.out = make([]Link, 0, min(sc.x.nhttp, 64))
	}
	sc.out = append(sc.out, l)
}
