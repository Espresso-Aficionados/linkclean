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
// closers are found through next-occurrence tables, never by rescanning (§8).
func Find(content string) []Link {
	links, _, _, _ := findSteps(content, 0)
	return links
}

// findSteps is Find with its work counters. budget > 0 stops the scan once
// more than budget steps were taken (aborted is then true and links is
// meaningless); steps counts every loop iteration and rescans every
// non-monotone cursor query. Find == findSteps(content, 0).
func findSteps(content string, budget int) (links []Link, steps, rescans int, aborted bool) {
	x := &index{s: content, budget: budget}
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

// scan scans the span [lo, hi) at nesting depth depth; spoiler reports
// whether an enclosing span is a spoiler.
//
// This body is a placeholder for the full grammar: it knows only bare links
// and ||spoilers||, so it agrees with Find's contract on content made of
// plain text, whitespace-separated bare http(s) links ending in a letter,
// digit or '/', and ||…|| spans with no other markup characters.
func (sc *scanner) scan(lo, hi int, spoiler bool, depth int) {
	s := sc.x.s
	for i := lo; i < hi && !sc.x.over(); {
		sc.x.steps++
		if s[i] == '|' && i+1 < hi && s[i+1] == '|' && depth < maxDepth {
			if c := strings.Index(s[min(i+3, hi):hi], "||"); c >= 0 {
				c += min(i+3, hi)
				sc.span++
				sc.scan(i+2, c, true, depth+1)
				i = c + 2
				continue
			}
		}
		if s[i] == 'h' {
			if e := bareEnd(s, i, hi); e >= 0 {
				sc.out = append(sc.out, Link{URL: s[i:e], Start: i, End: e, Kind: Bare, Spoiler: spoiler})
				i = e
				continue
			}
		}
		i++
	}
}
