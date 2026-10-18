package discordlink

import (
	"strings"
	"unicode/utf8"
)

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

// maxContent is the longest content Find scans: every table holds int32
// positions up to len(content), and the arena's size (at most 10 int32 per
// byte) must fit an int on 32-bit platforms too.
const maxContent = min(1<<31-1, int(^uint(0)>>1)/10)

// nextCache is a monotone cursor over one class of positions. Invariant: no
// position in [from, at) is a hit, and at is a hit or len(s). The zero value
// knows nothing beyond that vacuous claim: at == from is always re-checked.
type nextCache struct{ from, at int32 }

// index holds the per-call tables of one Find call and its work counters.
// There is no package-level mutable state, so concurrent calls never share
// one.
//
// Closers whose queries are monotone (the recursion visits openers in
// position order and every class queries at a fixed offset from its opener)
// use nextCache cursors, so each class crosses the string once. The
// masked-link classes are queried out of order and use next-occurrence
// arrays instead. Every table is a slice of one arena, sized by a prepass
// for the opener classes that occur and filled the first time it is used.
type index struct {
	s       string
	arena   []int32 // backing store of every table, sized once per call
	steps   int     // work done: every loop iteration counts one step
	rescans int     // cursor queries that went backwards (always a bug)
	budget  int     // > 0: stop working once steps exceeds it

	nhttp int   // occurrences of "http": the capacity of the first out slice
	spans int32 // span ids handed out; span-tagged memos compare against them

	// One cursor per closer class.
	strongC, uC, delC, spoilerC nextCache // "**" "__" "~~" "||"
	emStarC, emUnderC           nextCache // the last '*' or '_' of an odd run
	fenceC                      nextCache // "```"
	autoC, autoEndC             nextCache // ':', ' ', '>'; then ' ', '>'
	httpC                       nextCache // "http://" or "https://"

	// runs covers unescaped runs of '*' and of '_': at a run's first
	// position it holds the run's end, at its other positions the run's
	// start, and -1 at every position outside a run.
	runs     []int32
	esc      []int32 // bitset: bit p is set iff the byte at p is escaped
	runsDone bool

	// Raw backtick runs, bucketed by length: tickPos and tickNext per run
	// (its start, the next run of the same length or -1); tickID maps a
	// length to its bucket id + 1 (0: no run that long); per bucket,
	// tickFirst, tickCur (a cursor) and tickFrom (its last query).
	tickPos, tickNext, tickID    []int32
	tickFirst, tickCur, tickFrom []int32
	tickRuns, tickMax            int
	ticksDone                    bool

	// Masked links (the label chain and the href search).
	nClose, nBracket, nParen []int32 // first ']', first '[' or ']', first ')' at ≥ p
	nTitle                   []int32 // ')' of the first title close at ≥ p (a quote, JS ws*, ')')
	labTag, labVal           []int32 // label memo at positions after ']': span id, best candidate
	hrefTag, hrefVal         []int32 // href memo per target position: span id, suffix position or -1
	stack                    []int32 // the href DFS stack
	masked, maskedDone       bool
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

// nextSub is next for a class whose hits all start with sub: it jumps
// between occurrences of sub with strings.Index and calls hit (nil: every
// occurrence is a hit) only there. Steps count the positions passed, as in
// next.
func (x *index) nextSub(c *nextCache, from int, sub string, hit func(p int) bool) int {
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
	for p < n && !x.over() {
		j := strings.Index(x.s[p:], sub)
		if j < 0 {
			x.steps += n - p
			p = n
			break
		}
		x.steps += j + 1
		p += j
		if hit == nil || hit(p) {
			break
		}
		p++
	}
	c.from, c.at = int32(from), int32(p)
	return p
}

// escaped reports whether an odd run of '\' ends at p-1, i.e. whether the
// byte at p is escaped. Once buildRuns has run it reads the parity bitset;
// before that it walks the run.
func (x *index) escaped(p int) bool {
	if x.runsDone {
		return x.esc[p>>5]&(1<<(p&31)) != 0
	}
	k := 0
	for q := p - 1; q >= 0 && x.s[q] == '\\'; q-- {
		k++
	}
	x.steps += k
	return k%2 == 1
}

// prepare runs the prepass: it records which tables the content can need and
// carves them from one arena allocation. Their contents are built on first
// use.
func (x *index) prepare() {
	s := x.s
	n := len(s)
	var runs, open, link bool
	for i := 0; i < n; i++ {
		switch s[i] {
		case '*', '_':
			runs = true
		case '[':
			open = true
		case ']':
			if i+1 < n && s[i+1] == '(' {
				link = true
			}
		case '`':
			j := i + 1
			for j < n && s[j] == '`' {
				j++
			}
			x.tickRuns++
			x.tickMax = max(x.tickMax, j-i)
			i = j - 1
		}
	}
	x.steps += n
	x.masked = open && link
	size := 0
	if runs {
		size += n + n/32 + 1
	}
	buckets := min(x.tickRuns, x.tickMax)
	size += 2*x.tickRuns + 3*buckets
	if x.tickRuns > 0 {
		size += x.tickMax + 1
	}
	if x.masked {
		size += 9 * (n + 1)
	}
	if size == 0 {
		return
	}
	x.arena = make([]int32, size)
	a := x.arena
	take := func(k int) []int32 {
		t := a[:k:k]
		a = a[k:]
		return t
	}
	if runs {
		x.runs, x.esc = take(n), take(n/32+1)
	}
	if x.tickRuns > 0 {
		x.tickPos, x.tickNext = take(x.tickRuns), take(x.tickRuns)
		x.tickID = take(x.tickMax + 1)
		x.tickFirst, x.tickCur, x.tickFrom = take(buckets), take(buckets), take(buckets)
	}
	if x.masked {
		x.nClose, x.nBracket, x.nParen, x.nTitle = take(n+1), take(n+1), take(n+1), take(n+1)
		x.labTag, x.labVal = take(n+1), take(n+1)
		x.hrefTag, x.hrefVal = take(n+1), take(n+1)
		x.stack = take(n + 1)
	}
}

// buildRuns fills runs and the escape bitset.
func (x *index) buildRuns() {
	if x.runsDone {
		return
	}
	x.runsDone = true
	s, runs := x.s, x.runs
	start, bs := -1, 0 // start of the current run; '\' bytes directly before p
	for p := 0; p < len(s); p++ {
		b := s[p]
		if (b == '*' || b == '_') && bs%2 == 0 {
			if start >= 0 && s[start] == b {
				runs[p] = int32(start)
			} else {
				if start >= 0 {
					runs[start] = int32(p)
				}
				start = p
			}
		} else {
			if start >= 0 {
				runs[start] = int32(p)
				start = -1
			}
			runs[p] = -1
		}
		if bs%2 == 1 {
			x.esc[p>>5] |= 1 << (p & 31)
		}
		if b == '\\' {
			bs++
		} else {
			bs = 0
		}
	}
	if start >= 0 {
		runs[start] = int32(len(s))
	}
	x.steps += len(s)
}

// inRun reports whether p is in an unescaped run of '*' or '_'.
func (x *index) inRun(p int) bool { return x.runs[p] >= 0 }

// runStart and runEnd bound the run that contains p (inRun(p) must hold).
func (x *index) runStart(p int) int {
	if r := int(x.runs[p]); r < p {
		return r
	}
	return p
}

func (x *index) runEnd(p int) int { return int(x.runs[x.runStart(p)]) }

// oddRunEnd reports whether p is the last position of an odd run. Every
// such position stops an em search: it closes the em or blocks it.
func (x *index) oddRunEnd(p int) bool {
	if !x.inRun(p) {
		return false
	}
	st := x.runStart(p)
	return int(x.runs[st]) == p+1 && (p+1-st)%2 == 1
}

// buildTicks buckets the raw backtick runs by length.
func (x *index) buildTicks() {
	if x.ticksDone {
		return
	}
	x.ticksDone = true
	s := x.s
	r := 0
	for p := 0; p < len(s); {
		j := strings.IndexByte(s[p:], '`')
		if j < 0 {
			break
		}
		p += j
		q := p + 1
		for q < len(s) && s[q] == '`' {
			q++
		}
		x.tickPos[r], x.tickNext[r] = int32(p), int32(q-p) // tickNext holds the length for now
		r++
		p = q
	}
	ids := int32(0)
	for r := x.tickRuns - 1; r >= 0; r-- {
		l := x.tickNext[r]
		id := x.tickID[l]
		if id == 0 {
			ids++
			id = ids
			x.tickID[l] = id
			x.tickFirst[id-1] = -1
		}
		x.tickNext[r] = x.tickFirst[id-1]
		x.tickFirst[id-1] = int32(r)
	}
	copy(x.tickCur, x.tickFirst)
	x.steps += len(s) + x.tickRuns
}

// tickRun returns the start of the first raw backtick run of exactly k
// bytes that starts at or after from, or -1. Queries per length must have a
// non-decreasing from; a smaller one is answered by restarting that
// bucket's cursor and counts a rescan.
func (x *index) tickRun(k, from int) int {
	x.buildTicks()
	if k >= len(x.tickID) || x.tickID[k] == 0 {
		return -1
	}
	b := x.tickID[k] - 1
	if int32(from) < x.tickFrom[b] {
		x.rescans++
		x.tickCur[b] = x.tickFirst[b]
	}
	x.tickFrom[b] = int32(from)
	r := x.tickCur[b]
	for r >= 0 && int(x.tickPos[r]) < from {
		x.steps++
		r = x.tickNext[r]
	}
	x.tickCur[b] = r
	if r < 0 {
		return -1
	}
	return int(x.tickPos[r])
}

// buildMasked fills the masked-link next-occurrence arrays, right to left.
func (x *index) buildMasked() {
	if x.maskedDone {
		return
	}
	x.maskedDone = true
	s := x.s
	n := len(s)
	x.nClose[n], x.nBracket[n], x.nParen[n], x.nTitle[n] = int32(n), int32(n), int32(n), int32(n)
	for p := n - 1; p >= 0; p-- {
		x.nClose[p], x.nBracket[p], x.nParen[p], x.nTitle[p] = x.nClose[p+1], x.nBracket[p+1], x.nParen[p+1], x.nTitle[p+1]
		switch s[p] {
		case ']':
			x.nClose[p], x.nBracket[p] = int32(p), int32(p)
		case '[':
			x.nBracket[p] = int32(p)
		case ')':
			x.nParen[p] = int32(p)
		case '"', '\'':
			// A title close: the quote, JS whitespace, then ')'. Each
			// whitespace run is scanned only for the quote before it.
			if q := x.wsEnd(p+1, n); q < n && s[q] == ')' {
				x.nTitle[p] = int32(q)
			}
		}
	}
	x.steps += n
}

// wsEnd returns the end of the run of JS whitespace that starts at p,
// bounded by hi.
func (x *index) wsEnd(p, hi int) int {
	s := x.s
	for p < hi {
		x.steps++
		if b := s[p]; b < utf8.RuneSelf {
			if b != ' ' && (b < '\t' || b > '\r') {
				break
			}
			p++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[p:hi])
		if !isJSSpace(r) {
			break
		}
		p += size
	}
	return p
}
