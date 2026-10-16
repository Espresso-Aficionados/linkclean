package engine

import (
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// lre is a URLRegexps or PathRegexps entry with a prefilter: every match
// contains one of lits, so a string containing none of them is rejected with
// a few substring searches instead of a run of the backtracker over it.
type lre struct {
	*regexp.Regexp
	lits []string // nil: no prefilter
}

func compileLRE(expr string) (lre, error) {
	re, err := regexp.Compile(expr)
	if err != nil {
		return lre{}, err
	}
	l := lre{Regexp: re}
	if tree, err := syntax.Parse(expr, syntax.Perl); err == nil {
		l.lits = requiredLiterals(tree.Simplify())
	}
	return l, nil
}

// MatchString reports whether s contains a match of l.
func (l *lre) MatchString(s string) bool {
	if l.lits != nil && !slices.ContainsFunc(l.lits, func(lit string) bool { return strings.Contains(s, lit) }) {
		return false
	}
	return l.Regexp.MatchString(s)
}

const maxLits = 16 // the largest literal set worth testing

// requiredLiterals returns strings one of which every match of re contains,
// or nil when it finds no useful set.
func requiredLiterals(re *syntax.Regexp) []string {
	set, _ := literals(re)
	if set == nil || slices.Contains(set, "") {
		return nil
	}
	return set
}

// literals describes re by a set of at most maxLits strings. With exact set,
// re matches exactly the strings of the set; otherwise every match of re
// contains one of them. nil means nothing is known. Every string is UTF-8
// that a match contains byte for byte, so classes with non-ASCII runes give
// nil rather than enumerate them.
func literals(re *syntax.Regexp) (set []string, exact bool) {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return []string{""}, true
	case syntax.OpLiteral:
		// A case-folded rune stands for its whole SimpleFold orbit (k, K and
		// U+212A KELVIN SIGN). If the variants outgrow maxLits, a prefix of
		// the literal is still required.
		set = []string{""}
		for _, r := range re.Rune {
			if r == utf8.RuneError {
				// U+FFFD also matches an invalid byte, which Contains misses.
				return requiredOrNil(set)
			}
			alts := []string{string(r)}
			if re.Flags&syntax.FoldCase != 0 {
				for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
					alts = append(alts, string(f))
				}
			}
			if len(set)*len(alts) > maxLits {
				return requiredOrNil(set)
			}
			set = cross(set, alts)
		}
		return set, true
	case syntax.OpCharClass:
		var out []string
		for i := 0; i+1 < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			if hi >= utf8.RuneSelf || len(out)+int(hi-lo)+1 > 4 {
				return nil, false
			}
			for r := lo; r <= hi; r++ {
				out = append(out, string(r))
			}
		}
		if out == nil {
			return nil, false // an empty class never matches; nothing to gain
		}
		return out, true
	case syntax.OpCapture:
		return literals(re.Sub[0])
	case syntax.OpQuest:
		if set, exact := literals(re.Sub[0]); exact && len(set) < maxLits {
			return dedup(append(set, "")), true
		}
		return nil, false
	case syntax.OpPlus:
		set, _ := literals(re.Sub[0])
		return set, false
	case syntax.OpRepeat:
		if re.Min >= 1 {
			set, _ := literals(re.Sub[0])
			return set, false
		}
		return nil, false
	case syntax.OpAlternate:
		var out []string
		exact = true
		for _, sub := range re.Sub {
			set, e := literals(sub)
			if set == nil {
				return nil, false
			}
			out, exact = append(out, set...), exact && e
		}
		if out = dedup(out); len(out) > maxLits {
			return nil, false
		}
		return out, exact
	case syntax.OpConcat:
		return concatLiterals(re.Sub)
	}
	return nil, false
}

// concatLiterals is literals for a concatenation. A run of exact children is
// exact as a whole (the cross product of their sets), and the set of any run
// or of any child is necessary for the concatenation; the most selective of
// them is returned.
func concatLiterals(subs []*syntax.Regexp) ([]string, bool) {
	var best []string
	consider := func(set []string) {
		if set == nil || slices.Contains(set, "") {
			return
		}
		if best == nil || minLen(set) > minLen(best) || minLen(set) == minLen(best) && len(set) < len(best) {
			best = set
		}
	}
	run, allExact := []string{""}, true
	for _, sub := range subs {
		set, exact := literals(sub)
		switch {
		case exact && len(run)*len(set) <= maxLits:
			run = cross(run, set)
		case exact:
			consider(run)
			run, allExact = set, false
		default:
			consider(run)
			consider(set)
			run, allExact = []string{""}, false
		}
	}
	if allExact {
		return run, true
	}
	consider(run)
	return best, false
}

// requiredOrNil returns set as a non-exact set, or nil if it holds only "".
func requiredOrNil(set []string) ([]string, bool) {
	if slices.Contains(set, "") {
		return nil, false
	}
	return set, false
}

func cross(a, b []string) []string {
	out := make([]string, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			out = append(out, x+y)
		}
	}
	return dedup(out)
}

func dedup(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

func minLen(set []string) int {
	n := len(set[0])
	for _, s := range set[1:] {
		n = min(n, len(s))
	}
	return n
}
