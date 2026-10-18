package discordlink

// isJSSpace reports whether r is in JavaScript's \s class: the 25 code points
// U+0009–U+000D, U+0020, U+00A0, U+1680, U+2000–U+200A, U+2028, U+2029,
// U+202F, U+205F, U+3000 and U+FEFF. U+0085 and U+200B are not in it.
func isJSSpace(r rune) bool {
	if r < 0x80 {
		return r == ' ' || '\t' <= r && r <= '\r'
	}
	switch {
	case r == 0xA0, r == 0x1680, 0x2000 <= r && r <= 0x200A, r == 0x2028,
		r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000, r == 0xFEFF:
		return true
	}
	return false
}

// isLineTerm reports whether r is a JavaScript line terminator, which '.'
// does not match: '\n', '\r', U+2028 and U+2029.
func isLineTerm(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

// nextCache is a monotone cursor over one class of positions. Invariant: no
// position in [from, at) is a hit, and at is a hit or len(s). The zero value
// knows nothing beyond that vacuous claim: at == from is always re-checked.
type nextCache struct{ from, at int32 }

// index holds the per-call tables of one Find call and its work counters.
// There is no package-level mutable state, so concurrent calls never share
// one.
type index struct {
	s       string
	arena   []int32 // backing store of every table, sized once per call
	steps   int     // work done: every loop iteration counts one step
	rescans int     // cursor queries that went backwards (always a bug)
	budget  int     // > 0: stop working once steps exceeds it
}

// over reports whether the step budget is exhausted.
func (x *index) over() bool { return x.budget > 0 && x.steps > x.budget }

// next returns the first p ≥ from with hit(p), or len(s). It answers from c
// when it can and otherwise scans forward, counting one step per position.
// A query with from < c.from is still answered correctly, by rescanning, and
// counts one rescan. Once the budget is exhausted, answers are meaningless.
func (x *index) next(c *nextCache, from int, hit func(p int) bool) int {
	n := len(x.s)
	if from >= n {
		return n
	}
	switch {
	case from < int(c.from):
		x.rescans++
	case from < int(c.at):
		return int(c.at)
	}
	p := from
	for ; p < n && !x.over(); p++ {
		x.steps++
		if hit(p) {
			break
		}
	}
	c.from, c.at = int32(from), int32(p)
	return p
}

// escaped reports whether an odd run of '\' ends at p-1, i.e. whether the
// byte at p is escaped. This form walks the run; the table form is O(1).
func (x *index) escaped(p int) bool {
	k := 0
	for q := p - 1; q >= 0 && x.s[q] == '\\'; q-- {
		k++
	}
	return k%2 == 1
}
