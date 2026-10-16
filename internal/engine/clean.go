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

	nested := !inner && !c.opt.DisableNested
	if !inner && !c.opt.DisableUnwrap {
		cur := raw
		var seen []string
		abandoned := false
		for n := 0; ; n++ {
			t, ord, ok := cl.unwrapOnce()
			if !ok {
				break
			}
			if n == c.opt.MaxUnwrap || t == raw || slices.Contains(seen, t) {
				abandoned = true
				break
			}
			seen = append(seen, t)
			if rec != nil {
				rec.hops = append(rec.hops, cl.hop(cur, t, ord))
			}
			tu, sk := rawurl.Parse(t)
			if sk != rawurl.OK {
				// Unreachable: Reserialize output always parses. Stop here
				// rather than clean a URL the views do not describe.
				return t, NotSkipped
			}
			cur = t
			cl.reset(&tu)
			if cl.archived() {
				return t, NotSkipped // §5.5: an archive target is emitted uncleaned
			}
		}
		if abandoned {
			if rec != nil {
				rec.hops = nil
				rec.abandoned = true
			}
			nested = false
			if cur != raw {
				cl.reset(&u)
			}
		}
	}

	out, ok := cl.rounds(nested)
	if !ok {
		return raw, SkipRoundCap
	}
	return out, NotSkipped
}

// call is the state of cleaning one URL: the views that are constant for it
// (host, site, rule candidates), the current round's spans, and the scope
// memo. It lives on the stack of run.
type call struct {
	c   *Cleaner
	rec *recorder

	u        rawurl.URL // the URL as parsed; its host-derived views hold for every round
	t        matchpattern.Target
	siteDone bool

	// The candidate rules for u's host, ascending global ordinals, and the
	// scope memo of the current round per candidate: 0 unknown, 1 in scope,
	// 2 not. They live in cb and mb unless there are more than fit, in big
	// and bigMemo. cl never points into itself, so it stays on the stack.
	nc      int
	cb      [128]int32
	mb      [128]uint8
	big     []int32
	bigMemo []uint8

	// The current round's URL and spans. Strips never touch the authority,
	// so u.Prefix is a prefix of every round's raw.
	raw, path, query, frag string
	hasQuery, hasFrag      bool
	ms                     string // match string, valid iff msDone
	msDone                 bool
	signed                 bool
}

// reset points cl at a new URL, keeping its buffers.
func (cl *call) reset(u *rawurl.URL) {
	cl.u = *u
	cl.t = matchpattern.Target{Scheme: u.Scheme, Host: u.Host, IsIP: u.IsIP, Port: u.EffectivePort()}
	cl.siteDone = false
	var tmp [256]int32
	cands := tmp[:0]
	for i, p := range cl.c.progs {
		start := len(cands)
		cands, _ = p.hosts.Candidates(cands, u.Host, u.IsIP)
		if off := cl.c.offs[i]; off != 0 {
			for j := start; j < len(cands); j++ {
				cands[j] += off
			}
		}
	}
	cl.nc = len(cands)
	if cl.nc <= len(cl.cb) {
		copy(cl.cb[:], cands)
		cl.big, cl.bigMemo = nil, nil
	} else {
		cl.big = slices.Clone(cands)
		cl.bigMemo = make([]uint8, cl.nc)
	}
	cl.setRound(u.Raw, u.Path, u.Query, u.Fragment, u.HasQuery, u.HasFragment)
}

func (cl *call) setRound(raw, path, q, frag string, hasQuery, hasFrag bool) {
	cl.raw, cl.path, cl.query, cl.frag = raw, path, q, frag
	cl.hasQuery, cl.hasFrag = hasQuery, hasFrag
	cl.msDone = false
	clear(cl.memo())
	// pathForRequest (§4.1) is a substring of raw unless the path is empty.
	pfr := raw[len(cl.u.Prefix) : len(cl.u.Prefix)+len(path)]
	switch {
	case path == "" && hasQuery:
		pfr = "/?" + q
	case path == "":
		pfr = "/"
	case hasQuery:
		pfr = raw[len(cl.u.Prefix) : len(cl.u.Prefix)+len(path)+1+len(q)]
	}
	cl.t.SetPath(pfr)
}

// split re-derives the round spans of a URL that rounds rebuilt from
// cl.u.Prefix: rounds never write a '?' or '#' into the path, nor a '#' into
// the query.
func (cl *call) split(raw string) {
	tail := raw[len(cl.u.Prefix):]
	frag, hasFrag := "", false
	if i := strings.IndexByte(tail, '#'); i >= 0 {
		tail, frag, hasFrag = tail[:i], tail[i+1:], true
	}
	q, hasQuery := "", false
	if i := strings.IndexByte(tail, '?'); i >= 0 {
		tail, q, hasQuery = tail[:i], tail[i+1:], true
	}
	cl.setRound(raw, tail, q, frag, hasQuery, hasFrag)
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

func (cl *call) site() string {
	if !cl.siteDone {
		cl.siteDone = true
		if !cl.u.IsIP {
			cl.t.Site = rawurl.Site(cl.u.Host)
		}
	}
	return cl.t.Site
}

// matchString is the §4.1 match string of the current round. When the URL
// is written in canonical form (lowercase scheme, no userinfo or port, a
// host equal to its view, a non-empty path) it is the URL itself.
func (cl *call) matchString() string {
	if cl.msDone {
		return cl.ms
	}
	cl.msDone = true
	p, s, h := cl.u.Prefix, cl.u.Scheme, cl.u.Host
	if cl.path != "" && len(p) == len(s)+3+len(h) && p[:len(s)] == s && p[len(s)+3:] == h {
		cl.ms = cl.raw
		return cl.ms
	}
	var b strings.Builder
	b.Grow(len(cl.raw) + 1)
	b.WriteString(s)
	b.WriteString("://")
	b.WriteString(h)
	if port := cl.u.Port; port != "" && port != defaultPort(s) {
		b.WriteByte(':')
		b.WriteString(port)
	}
	if cl.path == "" {
		b.WriteByte('/')
	} else {
		b.WriteString(cl.path)
	}
	if cl.hasQuery {
		b.WriteByte('?')
		b.WriteString(cl.query)
	}
	if cl.hasFrag {
		b.WriteByte('#')
		b.WriteString(cl.frag)
	}
	cl.ms = b.String()
	return cl.ms
}

// inScope reports whether candidate cands[pos], rule r, is in scope for the
// current round (§4.2): an Include matches, no Exclude matches, and some
// URLRegexps entry matches if there are any. The answer is memoized for the
// round.
func (cl *call) inScope(pos int, r *crule) bool {
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
	cs := "" // string(cn), made only if a regexp needs it
	for pi, p := range cl.c.progs {
		off := cl.c.offs[pi]
		for _, lo := range p.params[string(cn)] {
			if cl.consider(off+lo, &p.rules[lo], frag, action, d) {
				return
			}
		}
		for _, lo := range p.regex {
			r := &p.rules[lo]
			if r.action != action || cl.candPos(off+lo) < 0 {
				continue
			}
			for i := range r.paramRes {
				pr := &r.paramRes[i]
				if !pr.prefixOK(cn) {
					continue
				}
				if cs == "" {
					cs = string(cn)
				}
				if pr.re.MatchString(cs) {
					if cl.consider(off+lo, r, frag, action, d) {
						return
					}
					break
				}
			}
		}
	}
}

// consider applies rule r (global ordinal ord) to d. It returns true when
// the answer can no longer change and nothing is recorded.
func (cl *call) consider(ord int32, r *crule, frag bool, action rule.Action, d *decision) bool {
	if r.action != action || action == rule.Strip && frag && !r.inFragment {
		return false
	}
	pos := cl.candPos(ord)
	if pos < 0 || !cl.c.isActive(ord) || !cl.inScope(pos, r) {
		return false
	}
	if action == rule.Keep {
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
// parameter (§5.5).
func (cl *call) isSigned(segs []query.Segment) bool {
	if cl.c.opt.DisableSignedGuard {
		return false
	}
	var nb [64]byte
	for i := range segs {
		n := segs[i].Name
		if n == "" || len(n) > len(nb) {
			continue
		}
		if _, ok := cl.c.signed[string(query.AppendCanon(nb[:0], n))]; ok {
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
			return cl.raw, true
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
	var sb [32]query.Segment
	segs := sb[:0]
	if cl.hasQuery {
		segs = query.Parse(segs, cl.query)
	}
	cl.signed = cl.isSigned(segs)

	path, pathChanged := cl.stripPath()

	var db [64]bool
	drop := db[:0]
	if len(segs) > len(db) {
		drop = make([]bool, len(segs))
	} else {
		drop = db[:len(segs)]
	}
	qChanged := false
	for i := range segs {
		s := &segs[i]
		d := cl.decide(s.Name, false)
		if d.removed() {
			drop[i] = true
			qChanged = true
			cl.recordRemoval(PartQuery, s, &d)
		} else {
			cl.recordKept(s.Name, &d)
		}
	}

	fragGone := false
	if cl.hasFrag && query.IsFragmentParams(cl.frag) {
		var fb [16]query.Segment
		fsegs := query.Parse(fb[:0], cl.frag)
		fragGone = true
		for i := range fsegs {
			if fsegs[i].Name == "" {
				continue
			}
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
				if fsegs[i].Name != "" {
					d := cl.decide(fsegs[i].Name, true)
					cl.recordRemoval(PartFragment, &fsegs[i], &d)
				}
			}
		}
	}

	// §5.4 nested values. The pass runs once per call (D4, P-6): a later
	// round sees a subset of these segments, each either unchanged by its
	// inner clean or re-encoded, which re-cleans to itself.
	var repl []replacement
	if !*nestedDone && !cl.signed {
		*nestedDone = true
		for i := range segs {
			s := &segs[i]
			if drop[i] || !s.HasEq || !hasHTTPScheme(s.Value) {
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
			qChanged = true
			if cl.rec != nil {
				for _, r := range inner.removed {
					r.Part = PartNested
					r.NestedIn = s.Name
					cl.rec.removed = append(cl.rec.removed, r)
				}
			}
		}
	}

	if !pathChanged && !qChanged && !fragGone {
		return cl.raw, false
	}
	b := make([]byte, 0, len(cl.raw))
	b = append(b, cl.u.Prefix...)
	b = append(b, path...)
	if qChanged {
		mark := len(b)
		b = append(b, '?')
		for i := range segs {
			text := segs[i].Text
			if len(repl) > 0 && repl[0].i == i {
				text, repl = repl[0].text, repl[1:]
			}
			if drop[i] || text == "" {
				continue
			}
			if len(b) > mark+1 {
				b = append(b, '&')
			}
			b = append(b, text...)
		}
		if len(b) == mark+1 {
			b = b[:mark]
		}
	} else if cl.hasQuery {
		b = append(b, '?')
		b = append(b, cl.query...)
	}
	if cl.hasFrag && !fragGone {
		b = append(b, '#')
		b = append(b, cl.frag...)
	}
	return string(b), true
}

// stripPath applies the PathRegexps of the in-scope Strip rules to the path
// span (§5.2), at most maxPathPasses times.
func (cl *call) stripPath() (string, bool) {
	path := cl.path
	if path == "" {
		return path, false
	}
	changed := false
	for pass := 0; pass < maxPathPasses; pass++ {
		np := path
		for pos, ord := range cl.cands() {
			r := cl.c.rule(ord)
			if len(r.pathRes) == 0 || !cl.c.isActive(ord) || cl.signed && r.global || !cl.inScope(pos, r) {
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
	if changed && path == "" && strings.HasPrefix(cl.path, "/") {
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

// hasHTTPScheme reports whether v, decoded once (§5.4), starts with
// "http://" or "https://" (ASCII case-insensitive), without decoding all of v.
func hasHTTPScheme(v string) bool {
	var b [8]byte
	n := 0
	for i := 0; i < len(v) && n < len(b); i++ {
		c := v[i]
		if c == '%' && i+2 < len(v) && isHex(v[i+1]) && isHex(v[i+2]) {
			c = unhex(v[i+1])<<4 | unhex(v[i+2])
			i += 2
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[n] = c
		n++
	}
	return n >= 7 && string(b[:7]) == "http://" || n == 8 && string(b[:8]) == "https://"
}

// hasHTTPPrefix reports whether s starts with "http://" or "https://",
// ASCII case-insensitive.
func hasHTTPPrefix(s string) bool {
	return len(s) >= 7 && strings.EqualFold(s[:7], "http://") || len(s) >= 8 && strings.EqualFold(s[:8], "https://")
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
