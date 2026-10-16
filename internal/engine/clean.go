package engine

import (
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

const (
	maxRounds     = 8 // §4.3 fixed-point cap
	maxPathPasses = 8 // §5.2
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
// of run.
type call struct {
	c   *Cleaner
	rec *recorder

	// u is the current round's URL. Rounds only edit the path, query and
	// fragment, so its host-derived views (Prefix, Host, Port, IsIP) hold
	// for every round; reset replaces them after an unwrap hop.
	u        rawurl.URL
	t        matchpattern.Target
	siteDone bool

	// The candidates whose scope must be evaluated (host-indexed, and global
	// with a non-trivial scope), ascending global ordinals, and the scope memo
	// of the current round per candidate: 0 unknown, 1 in scope, 2 not.
	// Trivial-scope rules are candidates for every URL and are not listed.
	// The specials are the candidates, trivial ones included, that cleaning
	// visits without a param name (crule.special), ascending. All of these
	// live in the fixed arrays unless they do not fit, in the big slices. cl
	// never points into itself, so it stays on the stack.
	nc      int
	cb      [128]int32
	mb      [128]uint8
	big     []int32
	bigMemo []uint8
	ns      int
	sb      [32]special
	bigSpec []special

	ms     string // match string, valid iff msDone
	msDone bool
	signed bool
	rxBuf  []byte // a name for ParamRegexps matching
}

// special is a candidate rule with ParamRegexps, PathRegexps or action
// Unwrap.
type special struct {
	r   *crule
	ord int32 // global ordinal
	pos int32 // index into the candidates and memo; -1 for a trivial scope
}

// reset points cl at a new URL, keeping its buffers.
func (cl *call) reset(u *rawurl.URL) {
	cl.u = *u
	cl.t = matchpattern.Target{Scheme: u.Scheme, Host: u.Host, IsIP: u.IsIP, Port: u.EffectivePort()}
	cl.siteDone = false
	cands, specs := cl.cb[:0], cl.sb[:0]
	for i, p := range cl.c.progs {
		off := cl.c.offs[i]
		start := len(cands)
		cands = p.hosts.Candidates(cands, u.Host, u.IsIP)
		// Merge this program's candidates and trivial specials by ordinal.
		for k, j := start, 0; k < len(cands) || j < len(p.trivial); {
			lo, pos := int32(0), int32(-1)
			if j == len(p.trivial) || k < len(cands) && cands[k] < p.trivial[j] {
				lo, pos = cands[k], int32(k)
				k++
			} else {
				lo = p.trivial[j]
				j++
			}
			if r := &p.rules[lo]; r.special() {
				specs = append(specs, special{r: r, ord: off + lo, pos: pos})
			}
		}
		for k := start; k < len(cands); k++ {
			cands[k] += off
		}
	}
	cl.nc, cl.ns = len(cands), len(specs)
	cl.big, cl.bigMemo, cl.bigSpec = nil, nil, nil
	if len(cands) > len(cl.cb) {
		cl.big = slices.Clone(cands)
		cl.bigMemo = make([]uint8, len(cands))
	}
	if len(specs) > len(cl.sb) {
		cl.bigSpec = slices.Clone(specs)
	}
	cl.newRound()
}

// newRound resets the per-round state for cl.u.
func (cl *call) newRound() {
	cl.msDone = false
	clear(cl.memo())
	cl.t.SetPath(cl.u.PathForRequest())
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

// cands returns the candidate list. The result points into cl: it must not
// be stored.
func (cl *call) cands() []int32 {
	if cl.big != nil {
		return cl.big
	}
	return cl.cb[:cl.nc]
}

func (cl *call) memo() []uint8 {
	if cl.big != nil {
		return cl.bigMemo
	}
	return cl.mb[:cl.nc]
}

// specials returns the special candidates. The result points into cl: it
// must not be stored.
func (cl *call) specials() []special {
	if cl.bigSpec != nil {
		return cl.bigSpec
	}
	return cl.sb[:cl.ns]
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

// inScope reports whether candidate cands[pos], rule r, is in scope for the
// current round (§4.2): an Include matches, no Exclude matches, and some
// URLRegexps entry matches if there are any. The answer is memoized for the
// round. pos < 0 means r's scope is trivial.
func (cl *call) inScope(pos int, r *crule) bool {
	if pos < 0 {
		return true
	}
	m := cl.memo()
	switch m[pos] {
	case 1:
		return true
	case 2:
		return false
	}
	ok := cl.evalScope(r)
	m[pos] = 2
	if ok {
		m[pos] = 1
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
	for _, re := range r.urlRes {
		if re.MatchString(ms) {
			return true
		}
	}
	return false
}

// candPos returns ord's position in cands, or -1.
func (cl *call) candPos(ord int32) int {
	if i, ok := slices.BinarySearch(cl.cands(), ord); ok {
		return i
	}
	return -1
}

// archived reports whether the archive guard (§5.5) applies to cl's URL.
func (cl *call) archived() bool {
	if cl.c.opt.DisableArchiveGuard {
		return false
	}
	for _, p := range builtinArchive[cl.u.Host] {
		if p.Match(&cl.t) {
			return true
		}
	}
	for _, p := range cl.c.archive {
		if k, _ := p.Host(); k == matchpattern.HostLabel {
			cl.site()
		}
		if p.Match(&cl.t) {
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
	cn := query.AppendCanon(nb[:0], name)
	cl.scan(cn, frag, rule.Strip, &d)
	if d.strip >= 0 || d.signedOnly {
		cl.scan(cn, frag, rule.Keep, &d)
	}
	return d
}

// scan considers every rule of the given action that names canonical name
// cn, through the params index or a ParamRegexps entry.
func (cl *call) scan(cn []byte, frag bool, action rule.Action, d *decision) {
	for pi, p := range cl.c.progs {
		off := cl.c.offs[pi]
		for _, lo := range p.params[string(cn)] {
			r := &p.rules[lo]
			if r.action != action {
				continue
			}
			pos := -1
			if !r.trivial {
				if pos = cl.candPos(off + lo); pos < 0 {
					continue
				}
			}
			if cl.consider(off+lo, pos, r, frag, d) {
				return
			}
		}
	}
	for _, sp := range cl.specials() {
		r := sp.r
		if r.action != action {
			continue
		}
		for i := range r.paramRes {
			pr := &r.paramRes[i]
			if !pr.prefixOK(cn) {
				continue
			}
			// Match would leak cn, a stack buffer, so the name goes through
			// one heap buffer per call instead of one string per name.
			cl.rxBuf = append(cl.rxBuf[:0], cn...)
			if pr.re.Match(cl.rxBuf) {
				if cl.consider(sp.ord, int(sp.pos), r, frag, d) {
					return
				}
				break
			}
		}
	}
}

// consider applies candidate rule r (global ordinal ord, candidate position
// pos) to d. It returns true when the answer can no longer change and
// nothing is recorded.
func (cl *call) consider(ord int32, pos int, r *crule, frag bool, d *decision) bool {
	if r.action == rule.Strip && frag && !r.inFragment {
		return false
	}
	if !cl.c.isActive(ord) || !cl.inScope(pos, r) {
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

// isSigned reports whether some segment's canonical name is a signature
// parameter (§5.5). Names of any length are compared: AppendCanon grows
// past the stack buffer when it has to.
func (cl *call) isSigned(segs []query.Segment) bool {
	if cl.c.opt.DisableSignedGuard {
		return false
	}
	var nb [64]byte
	for i := range segs {
		if _, ok := cl.c.signed[string(query.AppendCanon(nb[:0], segs[i].Name))]; ok {
			return true
		}
	}
	return false
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
// rebuilt URL and whether anything changed.
func (cl *call) round(nestedDone *bool) (string, bool) {
	u := &cl.u
	var sb [32]query.Segment
	var segs []query.Segment
	if u.HasQuery {
		segs = query.Parse(sb[:0], u.Query)
	}
	cl.signed = cl.isSigned(segs)

	path, changed := cl.stripPath()

	var db [32]bool
	drop := db[:]
	if len(segs) > len(db) {
		drop = make([]bool, len(segs))
	}
	for i := range segs {
		s := &segs[i]
		d := cl.decide(s.Name, false)
		if d.removed() {
			drop[i] = true
			changed = true
			cl.recordRemoval(PartQuery, s, &d)
		} else {
			cl.recordKept(s.Name, &d)
		}
	}

	// §5.3: IsFragmentParams guarantees every segment a non-empty name.
	fragGone := false
	if u.HasFragment && query.IsFragmentParams(u.Fragment) {
		var fb [16]query.Segment
		fsegs := query.Parse(fb[:0], u.Fragment)
		fragGone = true
		for i := range fsegs {
			if d := cl.decide(fsegs[i].Name, true); !d.removed() {
				fragGone = false
				if cl.rec == nil {
					break
				}
				cl.recordKept(fsegs[i].Name, &d)
			}
		}
		if fragGone && cl.rec != nil {
			for i := range fsegs {
				d := cl.decide(fsegs[i].Name, true)
				cl.recordRemoval(PartFragment, &fsegs[i], &d)
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
		for i := range segs {
			s := &segs[i]
			if drop[i] || !s.HasEq || !query.HasHTTPScheme(s.Value) {
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
	b := make([]byte, 0, len(u.Raw))
	b = append(b, u.Prefix...)
	b = append(b, path...)
	mark := len(b)
	for i := range segs {
		if drop[i] {
			continue
		}
		text := segs[i].Text
		if len(repl) > 0 && repl[0].i == i {
			text, repl = repl[0].text, repl[1:]
		}
		if len(b) == mark {
			b = append(b, '?')
		} else {
			b = append(b, '&')
		}
		b = append(b, text...)
	}
	if u.HasFragment && !fragGone {
		b = append(b, '#')
		b = append(b, u.Fragment...)
	}
	return string(b), true
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
		for _, sp := range cl.specials() {
			r := sp.r
			if len(r.pathRes) == 0 || !cl.c.isActive(sp.ord) || cl.signed && r.global || !cl.inScope(int(sp.pos), r) {
				continue
			}
			for _, re := range r.pathRes {
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
