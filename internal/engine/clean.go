package engine

import (
	"regexp"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/hostindex"
	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

const (
	maxRounds     = 8 // §4.3 fixed-point cap
	maxPathPasses = 8 // §5.2

	// stackRules is the most rules whose scope memo fits in a call.
	stackRules = 2048
)

// recorder collects what Explain reports. Clean passes nil and records
// nothing.
type recorder struct {
	hops      []Hop
	abandoned bool
	signed    bool
	removed   []Removal
	kept      []Kept
}

// Clean returns the cleaned URL. An unchanged input is returned as is.
func (c *Cleaner) Clean(raw string) string {
	out, _ := c.run(raw, nil, false)
	return out
}

// Explain cleans raw and reports what was done.
func (c *Cleaner) Explain(raw string) Result {
	var rec recorder
	out, skip := c.run(raw, &rec, false)
	if skip != NotSkipped {
		return Result{URL: raw, Skipped: skip}
	}
	return Result{
		URL:             out,
		Hops:            rec.hops,
		UnwrapAbandoned: rec.abandoned,
		Signed:          rec.signed,
		Removed:         rec.removed,
		Kept:            rec.kept,
	}
}

// run implements §4–§6 for one URL. inner marks the depth-1 clean of a
// nested value: no unwrapping and no nesting. A non-zero Skip means the
// input is returned unchanged and rec holds nothing meaningful.
//
// Unwrap runs before the strip rounds (§6) and once more after any round
// that changed the URL: a strip can bring an Unwrap rule into scope (an
// Include glob or URLRegexps that reads the query), and without the retry
// Clean(Clean(x)) would unwrap what Clean(x) did not (§5.6 invariant 1).
// Retried hops count toward MaxUnwrap and share the cycle check.
func (c *Cleaner) run(raw string, rec *recorder, inner bool) (string, Skip) {
	if len(raw) > c.opt.MaxURLLength {
		return raw, SkipTooLong
	}
	u, sk := rawurl.Parse(raw)
	switch sk {
	case rawurl.Scheme:
		return raw, SkipScheme
	case rawurl.Unparseable:
		return raw, SkipUnparseable
	case rawurl.Ambiguous:
		return raw, SkipAmbiguous
	}

	cl := call{c: c, rec: rec}
	if c.scopeWords > stackRules/64 {
		cl.bigScope = c.scopePool.Get().(*[]uint64)
		defer c.scopePool.Put(cl.bigScope)
	}
	cl.reset(&u)
	if cl.archived() {
		return raw, SkipArchive
	}

	unwrap := !inner && !c.opt.DisableUnwrap
	nested := !inner && !c.opt.DisableNested
	var seen []string
	cleaned := false // cl's URL is the fixed point of the rounds
	for {
		if unwrap {
			t, ord, ok := cl.unwrapOnce()
			switch {
			case ok && (len(seen) == c.opt.MaxUnwrap || t == raw || slices.Contains(seen, t)):
				// K8: abandon the chain and clean the input without nesting.
				unwrap, nested, cleaned = false, false, false
				if rec != nil {
					rec.hops, rec.removed, rec.abandoned = nil, nil, true
				}
				if cl.u.Raw != raw {
					cl.reset(&u)
				}
				continue
			case ok:
				seen = append(seen, t)
				if rec != nil {
					rec.hops = append(rec.hops, cl.hop(t, ord))
					// Rounds already run were on the wrapper, which is gone.
					rec.removed, rec.kept, rec.signed = nil, nil, false
				}
				tu, sk := rawurl.Parse(t)
				if sk != rawurl.OK {
					// Unreachable: Reserialize output always parses. Stop here
					// rather than clean a URL the views do not describe.
					return t, NotSkipped
				}
				cl.reset(&tu)
				if cl.archived() {
					return t, NotSkipped // §5.5: an archive target is emitted uncleaned
				}
				cleaned = false
				continue
			case cleaned:
				return cl.u.Raw, NotSkipped
			}
		}
		before := cl.u.Raw
		out, ok := cl.rounds(nested)
		if !ok {
			return raw, SkipRoundCap
		}
		if !unwrap || out == before {
			return out, NotSkipped
		}
		cleaned = true
	}
}

// call is the state of cleaning one URL: its views, the rule candidates for
// its host, and the scope memo of the current round. It lives on the stack
// of run; cl never points into itself, so it stays there.
type call struct {
	c   *Cleaner
	rec *recorder

	// u is the current round's URL. Rounds only edit the path, query and
	// fragment, so its host-derived views (Prefix, Host, Port, IsIP) hold
	// for every round; reset replaces them after an unwrap hop.
	u        rawurl.URL
	t        matchpattern.Target
	siteDone bool

	// The host-index buckets of u's host, per program, and two lists of the
	// rules they hold, as global ordinals, ascending: the special ones
	// (crule.special) and those with ParamRegexps. Each lives in its fixed
	// array unless it does not fit, in its big slice.
	nb   int
	bb   [16]hostBucket
	bigB []hostBucket
	ns   int
	sb   [32]int32
	bigS []int32
	nr   int
	rb   [16]int32
	bigR []int32

	// scope memoizes the current round's scope answers by global ordinal:
	// word i/64 of the first c.scopeWords words has bit i%64 set once rule i
	// was evaluated, and the same bit of the next c.scopeWords words if it
	// was in scope. bigScope replaces scope for a Cleaner of more than
	// stackRules rules. newRound clears the evaluated bits.
	scope    [2 * stackRules / 64]uint64
	bigScope *[]uint64

	ms      string // match string, valid iff msDone
	msDone  bool
	signed  bool
	nameBuf []byte   // a canonical name too long for a stack buffer
	rxBuf   []byte   // a name for ParamRegexps matching
	dropBuf []uint64 // round's drop bits past its stack buffer, reused by later rounds
}

type hostBucket struct {
	prog int
	b    *hostindex.Bucket
}

// reset points cl at a new URL, keeping its buffers.
func (cl *call) reset(u *rawurl.URL) {
	cl.u = *u
	cl.t = matchpattern.Target{Scheme: u.Scheme, Host: u.Host, IsIP: u.IsIP, Port: u.EffectivePort()}
	cl.siteDone = false
	var tmp [16]*hostindex.Bucket
	bks, sp, rx := cl.bb[:0], cl.sb[:0], cl.rb[:0]
	for pi, p := range cl.c.progs {
		bs := p.hosts.Lookup(tmp[:0], u.Host, u.IsIP)
		for _, b := range bs {
			bks = append(bks, hostBucket{pi, b})
		}
		sp = appendGlobal(sp, bs, hostindex.ListSpecial, cl.c.offs[pi])
		rx = appendGlobal(rx, bs, hostindex.ListParamRx, cl.c.offs[pi])
	}
	cl.nb, cl.ns, cl.nr = len(bks), len(sp), len(rx)
	cl.bigB, cl.bigS, cl.bigR = nil, nil, nil
	if len(bks) > len(cl.bb) {
		cl.bigB = slices.Clone(bks)
	}
	if len(sp) > len(cl.sb) {
		cl.bigS = slices.Clone(sp)
	}
	if len(rx) > len(cl.rb) {
		cl.bigR = slices.Clone(rx)
	}
	cl.newRound()
}

// appendGlobal appends list l of bs, the buckets of the program whose first
// rule has global ordinal off, to dst as global ordinals, ascending.
func appendGlobal(dst []int32, bs []*hostindex.Bucket, l hostindex.List, off int32) []int32 {
	start := len(dst)
	dst = hostindex.Merge(dst, bs, l)
	for k := start; k < len(dst); k++ {
		dst[k] += off
	}
	return dst
}

// newRound resets the per-round state for cl.u.
func (cl *call) newRound() {
	cl.msDone = false
	clear(cl.scopeBits()[:cl.c.scopeWords])
	if cl.u.Path == "" && cl.u.HasQuery {
		cl.t.SetRootPath(cl.u.Query)
	} else {
		cl.t.SetPath(cl.u.PathForRequest())
	}
}

// split points cl.u at raw, a URL that rounds rebuilt from cl.u.Prefix:
// rounds never write a '?' or '#' into the path, nor a '#' into the query.
func (cl *call) split(raw string) {
	u := &cl.u
	tail := raw[len(u.Prefix):]
	u.Raw = raw
	u.Fragment, u.HasFragment = "", false
	if i := strings.IndexByte(tail, '#'); i >= 0 {
		tail, u.Fragment, u.HasFragment = tail[:i], tail[i+1:], true
	}
	u.Query, u.HasQuery = "", false
	if i := strings.IndexByte(tail, '?'); i >= 0 {
		tail, u.Query, u.HasQuery = tail[:i], tail[i+1:], true
	}
	u.Path = tail
	cl.newRound()
}

// buckets, hostSpecials, hostRx and scopeBits return the views reset built.
// The results point into cl: they must not be stored.
func (cl *call) buckets() []hostBucket {
	if cl.bigB != nil {
		return cl.bigB
	}
	return cl.bb[:cl.nb]
}

func (cl *call) hostSpecials() []int32 {
	if cl.bigS != nil {
		return cl.bigS
	}
	return cl.sb[:cl.ns]
}

func (cl *call) hostRx() []int32 {
	if cl.bigR != nil {
		return cl.bigR
	}
	return cl.rb[:cl.nr]
}

func (cl *call) scopeBits() []uint64 {
	if cl.bigScope != nil {
		return *cl.bigScope
	}
	return cl.scope[:2*cl.c.scopeWords]
}

// specIter yields the special candidates in ascending global ordinal: the
// host's, merged with every program's globals. The two never overlap.
type specIter struct {
	cl    *call
	k     int // next in hostSpecials
	pi, j int // next global: progs[pi].globals[j]
}

func (it *specIter) next() (ord int32, r *crule, ok bool) {
	c := it.cl.c
	for it.pi < len(c.progs) && it.j == len(c.progs[it.pi].globals) {
		it.pi, it.j = it.pi+1, 0
	}
	hs := it.cl.hostSpecials()
	if it.pi < len(c.progs) {
		lo := c.progs[it.pi].globals[it.j]
		if g := c.offs[it.pi] + lo; it.k == len(hs) || g < hs[it.k] {
			it.j++
			return g, &c.progs[it.pi].rules[lo], true
		}
	}
	if it.k == len(hs) {
		return 0, nil, false
	}
	it.k++
	return hs[it.k-1], c.rule(hs[it.k-1]), true
}

func (cl *call) site() string {
	if !cl.siteDone {
		cl.siteDone = true
		if !cl.u.IsIP {
			cl.t.Site = rawurl.Site(cl.u.Host)
		}
	}
	return cl.t.Site
}

// matchString is the §4.1 match string of the current round.
func (cl *call) matchString() string {
	if !cl.msDone {
		cl.ms, cl.msDone = cl.u.MatchString(), true
	}
	return cl.ms
}

// isCand reports whether rule lo of program pi, r, is a candidate for u's
// host: it has a global Include, or the host index lists it.
func (cl *call) isCand(pi int, lo int32, r *crule) bool {
	if r.global {
		return true
	}
	for _, hb := range cl.buckets() {
		if hb.prog == pi {
			if _, ok := slices.BinarySearch(hb.b.All, lo); ok {
				return true
			}
		}
	}
	return false
}

// inScope reports whether candidate rule r, global ordinal ord, is in scope
// for the current round (§4.2): an Include matches, no Exclude matches, and
// some URLRegexps entry matches if there are any. Each rule is evaluated at
// most once per round.
func (cl *call) inScope(ord int32, r *crule) bool {
	if r.trivial {
		return true
	}
	bits := cl.scopeBits()
	w, m := int(ord/64), uint64(1)<<(ord%64)
	in := &bits[cl.c.scopeWords+w]
	if bits[w]&m != 0 {
		return *in&m != 0
	}
	ok := cl.evalScope(r)
	bits[w] |= m
	if ok {
		*in |= m
	} else {
		*in &^= m
	}
	return ok
}

func (cl *call) evalScope(r *crule) bool {
	if r.needSite {
		cl.site()
	}
	hit := false
	for _, p := range r.include {
		if p.Match(&cl.t) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, p := range r.exclude {
		if p.Match(&cl.t) {
			return false
		}
	}
	if len(r.urlRes) == 0 {
		return true
	}
	ms := cl.matchString()
	for i := range r.urlRes {
		if r.urlRes[i].MatchString(ms) {
			return true
		}
	}
	return false
}

// archived reports whether the archive guard (§5.5) applies to cl's URL.
// A run of '/' in the path is one separator to the archives' servers
// (web.archive.org serves "//web/…" as "/web/…"), so the guard also matches
// the path with each run collapsed to one '/'. Rule scopes do not.
func (cl *call) archived() bool {
	if cl.c.opt.DisableArchiveGuard {
		return false
	}
	if cl.matchArchive(&cl.t) {
		return true
	}
	p := cl.u.Path
	if !strings.Contains(p, "//") {
		return false
	}
	var b strings.Builder
	b.Grow(len(p) + 1 + len(cl.u.Query))
	for i := 0; i < len(p); i++ {
		if p[i] != '/' || i == 0 || p[i-1] != '/' {
			b.WriteByte(p[i])
		}
	}
	if cl.u.HasQuery {
		b.WriteByte('?')
		b.WriteString(cl.u.Query)
	}
	t := cl.t
	t.SetPath(b.String())
	return cl.matchArchive(&t)
}

func (cl *call) matchArchive(t *matchpattern.Target) bool {
	for _, p := range builtinArchive[t.Host] {
		if p.Match(t) {
			return true
		}
	}
	for _, p := range cl.c.opt.ExtraArchive {
		if k, _ := p.Host(); k == matchpattern.HostLabel {
			t.Site = cl.site()
		}
		if p.Match(t) {
			return true
		}
	}
	return false
}

// decision is the outcome of §4.3 step 6 for one name: the lowest in-scope
// Strip rule matching it, the lowest such affiliate rule, the lowest
// in-scope Keep rule, and whether a global Strip matched that the signed
// guard dropped. Ordinals are -1 when absent.
type decision struct {
	strip, aff, keep int32
	signedOnly       bool
}

func (d *decision) removed() bool { return d.strip >= 0 && d.keep < 0 }

// decide evaluates the rules that name a parameter, in fragment mode when
// frag is set (only InFragment Strip rules apply, §5.3). Keep rules are
// consulted only when some Strip rule matched.
func (cl *call) decide(name string, frag bool) decision {
	d := decision{strip: -1, aff: -1, keep: -1}
	if name == "" {
		return d
	}
	var nb [128]byte
	cn := cl.canon(&nb, name)
	cl.scan(cn, frag, rule.Strip, &d)
	if d.strip >= 0 || d.signedOnly {
		cl.scan(cn, frag, rule.Keep, &d)
	}
	return d
}

// canon returns canon(name) in nb, or in cl.nameBuf if it does not fit.
// The result is valid until the next canon call of a name longer than nb.
func (cl *call) canon(nb *[128]byte, name string) []byte {
	if len(name) <= len(nb) { // canon never lengthens a name
		return query.AppendCanon(nb[:0], name)
	}
	if cap(cl.nameBuf) < len(name) {
		cl.nameBuf = make([]byte, 0, len(name))
	}
	cl.nameBuf = query.AppendCanon(cl.nameBuf[:0], name)
	return cl.nameBuf
}

// scan considers every candidate rule of the given action that names
// canonical name cn: through the params index, a global rule's ParamRegexps
// entry, or a ParamRegexps entry of a rule the host index lists for u's
// host. Rules scoped to other hosts cost nothing.
func (cl *call) scan(cn []byte, frag bool, action rule.Action, d *decision) {
	inBuf := false // cl.rxBuf holds cn
	match := func(re *regexp.Regexp) bool {
		if !inBuf {
			// Match would leak cn, a stack buffer, so the name goes through
			// one heap buffer per call, not one string per name.
			cl.rxBuf, inBuf = append(cl.rxBuf[:0], cn...), true
		}
		return re.Match(cl.rxBuf)
	}
	for pi, p := range cl.c.progs {
		for _, lo := range p.params[string(cn)] {
			if r := &p.rules[lo]; r.action == action && cl.isCand(pi, lo, r) && cl.consider(pi, lo, r, frag, d) {
				return
			}
		}
		for _, n := range p.rxLens {
			if n > len(cn) {
				break
			}
			for _, e := range p.paramRx[string(cn[:n])] {
				if r := &p.rules[e.lo]; r.action == action && match(e.re) && cl.consider(pi, e.lo, r, frag, d) {
					return
				}
			}
		}
	}
	for _, ord := range cl.hostRx() {
		pi, lo := cl.c.locate(ord)
		r := &cl.c.progs[pi].rules[lo]
		if r.action != action {
			continue
		}
		for _, re := range r.paramRes {
			if pre, _ := re.LiteralPrefix(); len(pre) > len(cn) || string(cn[:len(pre)]) != pre || !match(re) {
				continue
			}
			if cl.consider(pi, lo, r, frag, d) {
				return
			}
			break
		}
	}
}

// consider applies candidate rule lo of program pi, r, to d if it is active
// and in scope. It returns true when the answer can no longer change and
// nothing is recorded.
func (cl *call) consider(pi int, lo int32, r *crule, frag bool, d *decision) bool {
	if r.action == rule.Strip && frag && !r.inFragment {
		return false
	}
	ord := cl.c.offs[pi] + lo
	if !cl.c.isActive(ord) || !cl.inScope(ord, r) {
		return false
	}
	if r.action == rule.Keep {
		if d.keep < 0 || ord < d.keep {
			d.keep = ord
		}
		return cl.rec == nil
	}
	if cl.signed && r.global {
		d.signedOnly = true
		return false
	}
	if d.strip < 0 || ord < d.strip {
		d.strip = ord
	}
	if r.affiliate && (d.aff < 0 || ord < d.aff) {
		d.aff = ord
	}
	return cl.rec == nil
}

// isSigned reports whether some segment of query q has a signature
// parameter as its canonical name (§5.5).
func (cl *call) isSigned(q string) bool {
	if cl.c.opt.DisableSignedGuard {
		return false
	}
	var nb [128]byte
	for {
		s, rest, ok := query.Next(q)
		if !ok {
			return false
		}
		if _, ok := cl.c.signed[string(cl.canon(&nb, s.Name))]; ok {
			return true
		}
		q = rest
	}
}

// rounds runs §4.3 rounds on cl's URL until one changes nothing. ok is
// false if that takes more than maxRounds rounds.
func (cl *call) rounds(nested bool) (out string, ok bool) {
	nestedDone := !nested
	for round := 1; ; round++ {
		if cl.rec != nil {
			cl.rec.kept = cl.rec.kept[:0]
		}
		next, changed := cl.round(&nestedDone)
		if !changed {
			if cl.rec != nil {
				cl.rec.signed = cl.signed
			}
			return cl.u.Raw, true
		}
		if round == maxRounds {
			return "", false
		}
		cl.split(next)
	}
}

type replacement struct {
	i    int
	text string
}

// round runs one §4.3 round: path, query, fragment, nested. It returns the
// rebuilt URL and whether anything changed. The query is walked segment by
// segment, four times at most (signed guard, decisions, nested values,
// rebuild), so its length costs no buffer beyond one bit per segment. The
// fragment is walked the same way.
func (cl *call) round(nestedDone *bool) (string, bool) {
	u := &cl.u
	cl.signed = cl.isSigned(u.Query)

	path, changed := cl.stripPath()

	// drop has bit i set for the i-th non-empty segment being removed.
	var db [32]uint64
	drop := db[:]
	if len(u.Query)/2+1 > 64*len(db) {
		if n := (query.Count(u.Query) + 63) / 64; n > len(db) {
			if cap(cl.dropBuf) < n {
				cl.dropBuf = make([]uint64, n)
			}
			drop = cl.dropBuf[:n]
			clear(drop)
		}
	}
	dropped := func(i int) bool { return drop[i/64]&(1<<(i%64)) != 0 }
	for i, q := 0, u.Query; ; i++ {
		s, rest, ok := query.Next(q)
		if !ok {
			break
		}
		q = rest
		d := cl.decide(s.Name, false)
		if d.removed() {
			drop[i/64] |= 1 << (i % 64)
			changed = true
			cl.recordRemoval(PartQuery, &s, &d)
		} else {
			cl.recordKept(s.Name, &d)
		}
	}

	// §5.3: the fragment goes only if every segment does. IsFragmentParams
	// guarantees every segment a non-empty name.
	fragGone := false
	if u.HasFragment && query.IsFragmentParams(u.Fragment) {
		fragGone = true
		for q := u.Fragment; ; {
			s, rest, ok := query.Next(q)
			if !ok {
				break
			}
			q = rest
			if d := cl.decide(s.Name, true); !d.removed() {
				fragGone = false
				if cl.rec == nil {
					break
				}
				cl.recordKept(s.Name, &d)
			}
		}
		if fragGone && cl.rec != nil {
			for q := u.Fragment; ; {
				s, rest, ok := query.Next(q)
				if !ok {
					break
				}
				q = rest
				d := cl.decide(s.Name, true)
				cl.recordRemoval(PartFragment, &s, &d)
			}
		}
		changed = changed || fragGone
	}

	// §5.4 nested values. The pass runs once per call (D4, P-6): a later
	// round sees a subset of these segments, each either unchanged by its
	// inner clean or re-encoded, which re-cleans to itself.
	var repl []replacement
	if !*nestedDone && !cl.signed {
		*nestedDone = true
		for i, q := 0, u.Query; ; i++ {
			s, rest, ok := query.Next(q)
			if !ok {
				break
			}
			q = rest
			if dropped(i) || !s.HasEq || !query.HasHTTPScheme(s.Value) {
				continue
			}
			t := query.DecodeValue(s.Value)
			var inner *recorder
			if cl.rec != nil {
				inner = &recorder{}
			}
			out, skip := cl.c.run(t, inner, true)
			if skip != NotSkipped || out == t {
				continue
			}
			repl = append(repl, replacement{i, s.Name + "=" + query.EncodeComponent(out)})
			changed = true
			if cl.rec != nil {
				// Inner path removals keep CatPathSegment (§3 PartNested).
				// Inner Kept entries are not reported.
				for _, r := range inner.removed {
					r.Part = PartNested
					r.NestedIn = s.Name
					cl.rec.removed = append(cl.rec.removed, r)
				}
			}
		}
	}

	if !changed {
		return u.Raw, false
	}
	// §5.1: once anything changed, the query is rebuilt from the surviving
	// non-empty segments, and '?' is written only if one survives.
	var b strings.Builder
	b.Grow(len(u.Raw))
	b.WriteString(u.Prefix)
	b.WriteString(path)
	mark := b.Len()
	for i, q := 0, u.Query; ; i++ {
		s, rest, ok := query.Next(q)
		if !ok {
			break
		}
		q = rest
		if dropped(i) {
			continue
		}
		text := s.Text
		if len(repl) > 0 && repl[0].i == i {
			text, repl = repl[0].text, repl[1:]
		}
		if b.Len() == mark {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(text)
	}
	if u.HasFragment && !fragGone {
		b.WriteByte('#')
		b.WriteString(u.Fragment)
	}
	return b.String(), true
}

// stripPath applies the PathRegexps of the in-scope Strip rules to the path
// span (§5.2), at most maxPathPasses times.
func (cl *call) stripPath() (string, bool) {
	path := cl.u.Path
	if path == "" {
		return path, false
	}
	changed := false
	for pass := 0; pass < maxPathPasses; pass++ {
		np := path
		for it := (specIter{cl: cl}); ; {
			ord, r, ok := it.next()
			if !ok {
				break
			}
			if len(r.pathRes) == 0 || !cl.c.isActive(ord) || cl.signed && r.global || !cl.inScope(ord, r) {
				continue
			}
			for i := range r.pathRes {
				re := &r.pathRes[i]
				if !re.MatchString(np) {
					continue
				}
				if cl.rec != nil {
					for _, m := range re.FindAllString(np, -1) {
						name, value, _ := strings.Cut(m, "=")
						cl.rec.removed = append(cl.rec.removed, Removal{
							Part: PartPath, Category: CatPathSegment,
							Name: name, Value: value, Text: m, Rule: r.id,
						})
					}
				}
				np = re.ReplaceAllLiteralString(np, "")
			}
		}
		if np == path {
			break
		}
		path, changed = np, true
	}
	if changed && path == "" && strings.HasPrefix(cl.u.Path, "/") {
		path = "/"
	}
	return path, changed
}

// recordRemoval records the removal of segment s per §3.1.
func (cl *call) recordRemoval(part Part, s *query.Segment, d *decision) {
	if cl.rec == nil {
		return
	}
	rm := Removal{Part: part, Category: CatTracker, Name: s.Name, Value: s.Value, Text: s.Text}
	if d.aff >= 0 {
		r := cl.c.rule(d.aff)
		rm.Category = CatAffiliate
		rm.Rule = r.id
		rm.Beneficiary.Network = r.network
		if r.idParam != "" && s.Value != "" && query.Canon(s.Name) == r.idParam {
			rm.Beneficiary.ID = query.DecodeValue(s.Value)
		}
	} else {
		rm.Rule = cl.c.rule(d.strip).id
	}
	cl.rec.removed = append(cl.rec.removed, rm)
}

// recordKept records a segment a Strip rule matched but that stays.
func (cl *call) recordKept(name string, d *decision) {
	if cl.rec == nil || d.strip < 0 && !d.signedOnly {
		return
	}
	switch {
	case d.keep >= 0:
		r := cl.c.rule(d.keep)
		reason := KeptByRule
		if r.affiliate {
			reason = KeptAffiliate
		}
		cl.rec.kept = append(cl.rec.kept, Kept{Name: name, Reason: reason, Rule: r.id})
	case d.strip < 0:
		cl.rec.kept = append(cl.rec.kept, Kept{Name: name, Reason: KeptSignedGuard})
	}
}

// hasHTTPPrefix reports whether s starts with "http://" or "https://",
// ASCII case-insensitive.
func hasHTTPPrefix(s string) bool {
	return len(s) >= 7 && strings.EqualFold(s[:7], "http://") || len(s) >= 8 && strings.EqualFold(s[:8], "https://")
}
