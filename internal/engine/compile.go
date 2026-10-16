package engine

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/hostindex"
	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// Program is a compiled, immutable list of rules from one or more rule sets,
// with the indexes cleaning needs. A rule's ordinal is its position in the
// list: sets in the order given, rules in set order. Safe for concurrent use.
type Program struct {
	rules []crule
	sets  []SetInfo
	// hosts indexes the rules without a global Include by Include host, and
	// marks the special ones (crule.special). A rule with a global Include
	// is a candidate for every URL; globals lists the special ones,
	// ascending.
	hosts   *hostindex.Index
	globals []int32
	params  map[string][]int32 // canon(name) -> Strip and Keep rules naming it, ascending
	// paramRx holds the ParamRegexps entries of the rules with a global
	// Include, keyed by literal prefix. A name can only match an entry whose
	// prefix it starts with, so it probes one key per distinct prefix length
	// (rxLens, ascending; 0 for entries without a prefix). The host index
	// lists the other rules with ParamRegexps (Bucket.ParamRx), so a URL
	// visits only its own host's.
	paramRx map[string][]rxEntry
	rxLens  []int
}

// rxEntry is one ParamRegexps entry of rule lo, anchored.
type rxEntry struct {
	lo int32
	re *regexp.Regexp
}

// SetInfo describes one compiled rule set.
type SetInfo struct {
	Name, License, Upstream, Commit, SHA256, Notice string
	Rules, Params, Regexps                          int
}

// crule is one compiled rule.
type crule struct {
	id        string // full ID, "<set>:<id>"
	tags      []string
	action    rule.Action
	affiliate bool
	global    bool // some Include has host "*": a candidate for every URL, and the signed guard's global bucket
	needSite  bool // some Include or Exclude has a label.* host
	trivial   bool // in scope for every URL: an Include "*://*/*", no Exclude, no URLRegexps

	include, exclude []*matchpattern.Pattern
	urlRes           []lre

	paramRes   []*regexp.Regexp // anchored; also in Program.paramRx if global
	inFragment bool
	pathRes    []lre

	network string
	idParam string // canonical
	idPath  *regexp.Regexp

	unwrapParams  []string // canonical
	allowSameSite bool
	prependScheme string
}

// special reports whether r acts without naming a param, so cleaning visits
// it for every URL it is a candidate for.
func (r *crule) special() bool {
	return len(r.pathRes) > 0 || r.action == rule.Unwrap
}

// Compile validates every rule of sets and compiles them in order. Rules of a
// set whose name starts with "brave/" may not use label.* hosts (§4.2). All
// problems are returned together, as an errors.Join of *rule.Error. Set names
// are the caller's to validate.
func Compile(sets []rule.Set) (*Program, error) {
	p := &Program{params: map[string][]int32{}}
	var b hostindex.Builder
	var errs []error
	for si := range sets {
		s := &sets[si]
		info := SetInfo{
			Name: s.Name, License: s.License, Upstream: s.Upstream,
			Commit: s.Commit, SHA256: s.SHA256, Notice: s.Notice,
		}
		allowLabel := !strings.HasPrefix(s.Name, "brave/")
		ids := make(map[string]bool, len(s.Rules))
		for i := range s.Rules {
			r := &s.Rules[i]
			info.Params += len(r.Params)
			info.Regexps += len(r.URLRegexps) + len(r.ParamRegexps) + len(r.PathRegexps)
			if r.IDPathRegexp != "" {
				info.Regexps++
			}
			e := ruleErrs{set: s.Name, id: r.ID}
			if r.ID == "" {
				e.id = fmt.Sprintf("#%d", i)
				e.add("id", errors.New("missing"))
			} else if ids[r.ID] {
				e.add("id", errors.New("duplicate rule ID in set"))
			}
			ids[r.ID] = true
			cr := compileRule(s.Name, r, allowLabel, &e)
			if len(e.errs) > 0 {
				errs = append(errs, e.errs...)
				continue
			}
			p.add(cr, r.Params, &b)
			info.Rules++
		}
		p.sets = append(p.sets, info)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	p.hosts = b.Build()
	for k, l := range p.params {
		p.params[k] = slices.Clip(l)
	}
	for k, l := range p.paramRx {
		p.paramRx[k] = slices.Clip(l)
		p.rxLens = append(p.rxLens, len(k))
	}
	slices.Sort(p.rxLens)
	p.rxLens = slices.Compact(p.rxLens)
	return p, nil
}

// add appends cr, compiled from a rule with the given Params, to p and
// indexes it.
func (p *Program) add(cr crule, params []string, b *hostindex.Builder) {
	ord := int32(len(p.rules))
	p.rules = append(p.rules, cr)
	switch {
	case !cr.global:
		for _, inc := range cr.include {
			b.Add(ord, inc, cr.special(), len(cr.paramRes) > 0)
		}
	case cr.special():
		p.globals = append(p.globals, ord)
	}
	for _, n := range params {
		cn := query.Canon(n)
		if l := p.params[cn]; len(l) == 0 || l[len(l)-1] != ord {
			p.params[cn] = append(l, ord)
		}
	}
	if !cr.global {
		return
	}
	for _, re := range cr.paramRes {
		if p.paramRx == nil {
			p.paramRx = map[string][]rxEntry{}
		}
		prefix, _ := re.LiteralPrefix()
		p.paramRx[prefix] = append(p.paramRx[prefix], rxEntry{lo: ord, re: re})
	}
}

// Sets describes the compiled rule sets, in order.
func (p *Program) Sets() []SetInfo { return slices.Clone(p.sets) }

// ruleErrs collects the problems of one rule.
type ruleErrs struct {
	set, id string
	errs    []error
}

func (e *ruleErrs) add(field string, err error) {
	e.errs = append(e.errs, &rule.Error{Set: e.set, RuleID: e.id, Field: field, Err: err})
}

// compileRule validates r (§3 Rule, §4) and compiles it. Problems go to e;
// the returned rule is meaningful only when e is empty.
func compileRule(set string, r *rule.Rule, allowLabel bool, e *ruleErrs) crule {
	cr := crule{
		id:         set + ":" + r.ID,
		tags:       slices.Clone(r.Tags),
		action:     r.Action,
		affiliate:  r.Affiliate,
		inFragment: r.InFragment,
		network:    r.Network,
	}
	for i, t := range r.Tags {
		if t == "" {
			e.add(fmt.Sprintf("tags[%d]", i), errors.New("empty tag"))
		}
	}

	if len(r.Include) == 0 {
		e.add("include", errors.New("at least one pattern is required (\"*://*/*\" is the global scope)"))
	}
	cr.include = patterns(r.Include, "include", allowLabel, e)
	cr.exclude = patterns(r.Exclude, "exclude", allowLabel, e)
	for _, m := range cr.include {
		switch k, _ := m.Host(); k {
		case matchpattern.HostAny:
			cr.global = true
		case matchpattern.HostLabel:
			cr.needSite = true
		}
	}
	for _, m := range cr.exclude {
		if k, _ := m.Host(); k == matchpattern.HostLabel {
			cr.needSite = true
		}
	}
	for i, s := range r.URLRegexps {
		if re, err := compileLRE(s); err != nil {
			e.add(fmt.Sprintf("urlRegexps[%d]", i), err)
		} else {
			cr.urlRes = append(cr.urlRes, re)
		}
	}
	cr.trivial = len(r.Exclude) == 0 && len(r.URLRegexps) == 0 &&
		slices.ContainsFunc(cr.include, (*matchpattern.Pattern).MatchesAll)

	switch r.Action {
	case rule.Strip, rule.Keep, rule.Unwrap:
	default:
		// Every check below depends on the action: report this alone.
		e.add("action", fmt.Errorf("invalid action %d", uint8(r.Action)))
		return cr
	}
	if r.Action == rule.Unwrap {
		if len(r.Params) > 0 {
			e.add("params", errors.New("only Strip and Keep rules match params"))
		}
		if len(r.ParamRegexps) > 0 {
			e.add("paramRegexps", errors.New("only Strip and Keep rules match params"))
		}
		if r.InFragment {
			e.add("inFragment", errors.New("only Strip and Keep rules match params"))
		}
	}
	for i, n := range r.Params {
		if n == "" {
			e.add(fmt.Sprintf("params[%d]", i), errors.New("empty name"))
		}
	}
	for i, s := range r.ParamRegexps {
		re, err := regexp.Compile("^(?:" + s + ")$")
		if err != nil {
			e.add(fmt.Sprintf("paramRegexps[%d]", i), err)
			continue
		}
		cr.paramRes = append(cr.paramRes, re)
	}
	if len(r.PathRegexps) > 0 && r.Action != rule.Strip {
		e.add("pathRegexps", errors.New("only Strip rules edit the path"))
	}
	for i, s := range r.PathRegexps {
		re, err := compileLRE(s)
		switch {
		case err != nil:
			e.add(fmt.Sprintf("pathRegexps[%d]", i), err)
		case re.Regexp.MatchString(""):
			e.add(fmt.Sprintf("pathRegexps[%d]", i), errors.New("matches the empty string"))
		default:
			cr.pathRes = append(cr.pathRes, re)
		}
	}
	switch r.Action {
	case rule.Strip:
		if len(r.Params) == 0 && len(r.ParamRegexps) == 0 && len(r.PathRegexps) == 0 {
			e.add("params", errors.New("a Strip rule needs params, paramRegexps or pathRegexps"))
		}
	case rule.Keep:
		if len(r.Params) == 0 && len(r.ParamRegexps) == 0 {
			e.add("params", errors.New("a Keep rule needs params or paramRegexps"))
		}
	}

	if r.Action == rule.Unwrap {
		compileUnwrap(r.Unwrap, &cr, e)
	} else if r.Unwrap != nil {
		e.add("unwrap", errors.New("only Unwrap rules have an unwrap spec"))
	}

	compileAffiliate(r, &cr, e)
	return cr
}

func patterns(ps []string, field string, allowLabel bool, e *ruleErrs) []*matchpattern.Pattern {
	out := make([]*matchpattern.Pattern, 0, len(ps))
	for i, s := range ps {
		m, err := matchpattern.Parse(s, allowLabel)
		if err != nil {
			e.add(fmt.Sprintf("%s[%d]", field, i), err)
			continue
		}
		out = append(out, m)
	}
	return out
}

func compileUnwrap(u *rule.UnwrapSpec, cr *crule, e *ruleErrs) {
	if u == nil {
		e.add("unwrap", errors.New("an Unwrap rule needs an unwrap spec"))
		return
	}
	if len(u.QueryParams) == 0 {
		e.add("unwrap.queryParams", errors.New("at least one name is required"))
	}
	for i, n := range u.QueryParams {
		if n == "" {
			e.add(fmt.Sprintf("unwrap.queryParams[%d]", i), errors.New("empty name"))
		}
		cr.unwrapParams = append(cr.unwrapParams, query.Canon(n))
	}
	switch u.PrependScheme {
	case "", "http", "https":
	default:
		e.add("unwrap.prependScheme", fmt.Errorf("%q is not \"http\" or \"https\"", u.PrependScheme))
	}
	cr.allowSameSite = u.AllowSameSite
	cr.prependScheme = u.PrependScheme
}

// compileAffiliate checks the affiliate data of §3 and §4.4.1.
func compileAffiliate(r *rule.Rule, cr *crule, e *ruleErrs) {
	if !r.Affiliate {
		for _, f := range [...]struct{ name, v string }{{"network", r.Network}, {"idParam", r.IDParam}, {"idPathRegexp", r.IDPathRegexp}} {
			if f.v != "" {
				e.add(f.name, errors.New("only affiliate rules carry network data"))
			}
		}
		return
	}
	if len(r.PathRegexps) > 0 {
		e.add("affiliate", errors.New("an affiliate rule cannot have pathRegexps"))
	}
	if r.Network != "" && !rule.NetworkRe.MatchString(r.Network) {
		e.add("network", fmt.Errorf("%q does not match %s", r.Network, rule.NetworkRe))
	}
	if r.IDParam != "" && r.IDPathRegexp != "" {
		e.add("idPathRegexp", errors.New("idParam and idPathRegexp are exclusive"))
	}
	if r.IDParam != "" {
		cr.idParam = query.Canon(r.IDParam)
		if r.Action != rule.Unwrap && !slices.ContainsFunc(r.Params, func(n string) bool { return query.Canon(n) == cr.idParam }) {
			e.add("idParam", fmt.Errorf("%q is not one of params", r.IDParam))
		}
	}
	if r.IDPathRegexp != "" {
		if r.Action != rule.Unwrap {
			e.add("idPathRegexp", errors.New("only Unwrap rules read an ID from the path"))
		}
		re, err := regexp.Compile(r.IDPathRegexp)
		switch {
		case err != nil:
			e.add("idPathRegexp", err)
		case re.NumSubexp() != 1:
			e.add("idPathRegexp", fmt.Errorf("has %d capture groups, want exactly 1", re.NumSubexp()))
		default:
			cr.idPath = re
		}
	}
}
