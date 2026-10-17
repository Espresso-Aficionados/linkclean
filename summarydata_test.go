package linkclean

import (
	"encoding/json"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// summaryData is what `rulegen summary` needs from the engine (§11.1), which
// the stdlib-only tools module cannot import: TestSummaryData writes it as
// JSON, and summary runs that test through `go test`. Field names are the
// contract; schema changes with any incompatible change.
type summaryData struct {
	Schema    int    `json:"schema"`     // 1
	BaseError string `json:"base_error"` // "" when base's data compiles; "no base" without LINKCLEAN_BASE_DIR
	HeadError string `json:"head_error"` // "" when head's data compiles

	// Corpus is every eval corpus item, cleaned under AffiliateKeep at base
	// and at head (the head engine over each side's data). Outputs are ""
	// on a side that does not compile.
	Corpus []summaryCorpusItem `json:"corpus"`

	// CorpusScopes maps every include pattern of base's and head's data
	// (Brave files and overlay), and "global", to whether any corpus input
	// matches it: the ★ flag.
	CorpusScopes map[string]bool `json:"corpus_scopes"`

	// Rule IDs are "<set>:<id>", sorted, of every compiled rule.
	BaseRuleIDs []string `json:"base_rule_ids"`
	HeadRuleIDs []string `json:"head_rule_ids"`

	// Debounce groups map each compiled brave/debounce rule ID to its
	// include patterns, so an ID can be traced to its patterns.
	BaseDebounceGroups map[string][]string `json:"base_debounce_groups"`
	HeadDebounceGroups map[string][]string `json:"head_debounce_groups"`

	// Hygiene is the overlay hygiene of head's data (summary only).
	Hygiene []summaryHygiene `json:"hygiene"`

	// AffiliateHosts maps the host of every affiliate-classed debounce
	// pattern at head (without a leading "*.") to its network: a stub's
	// "known network domain" is such a host or a subdomain of one.
	AffiliateHosts map[string]string `json:"affiliate_hosts"`
}

type summaryCorpusItem struct {
	ID         string   `json:"id"`
	Input      string   `json:"input"`
	Expected   string   `json:"expected"`
	Alt        []string `json:"alt_expected"`
	BaseOutput string   `json:"base_output"`
	HeadOutput string   `json:"head_output"`
	BaseBroken bool     `json:"base_broken"` // §11.3 rules-golden (b)
	HeadBroken bool     `json:"head_broken"`
}

// summaryHygiene is one overlay hygiene note.
type summaryHygiene struct {
	// Kind is "stale_keep" (a Keep overriding upstream whose name no Brave
	// strip rule strips in its scope any more), "duplicated_strip" (an overlay Strip name that
	// Brave strips in every scope of the rule) or "overlap" (an overlay
	// Unwrap include that a brave/debounce rule also covers).
	Kind   string `json:"kind"`
	Rule   string `json:"rule"`            // overlay rule ID
	Param  string `json:"param,omitempty"` // canonical name (stale_keep, duplicated_strip)
	Scope  string `json:"scope,omitempty"` // overlay include (overlap)
	With   string `json:"with,omitempty"`  // the Brave rule, "<set>:<id>" (duplicated_strip, overlap)
	AtBase bool   `json:"at_base"`         // the same note holds at base
}

// TestSummaryData writes the summary data to $LINKCLEAN_SUMMARY_DATA, with
// base from $LINKCLEAN_BASE_DIR. It is a no-op for every other run.
func TestSummaryData(t *testing.T) {
	out := os.Getenv("LINKCLEAN_SUMMARY_DATA")
	if out == "" {
		t.Skip("LINKCLEAN_SUMMARY_DATA not set")
	}
	var base fs.FS
	if dir := os.Getenv("LINKCLEAN_BASE_DIR"); dir != "" {
		base = os.DirFS(dir)
	}
	sd := buildSummaryData(t, rulesFS, base, os.DirFS("."))
	data, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// summarySide is one side's compiled data.
type summarySide struct {
	srcs []RuleSet
	c    *Cleaner
	err  string
}

func compileSide(fsys fs.FS) summarySide {
	if fsys == nil {
		return summarySide{err: "no base"}
	}
	srcs, err := sourcesFrom(fsys)
	if err != nil {
		return summarySide{err: err.Error()}
	}
	c, err := New(Config{Sources: srcs})
	if err != nil {
		return summarySide{err: err.Error()}
	}
	return summarySide{srcs: srcs, c: c}
}

func buildSummaryData(t testing.TB, head, base, tree fs.FS) summaryData {
	t.Helper()
	h, b := compileSide(head), compileSide(base)
	sd := summaryData{Schema: 1, BaseError: b.err, HeadError: h.err,
		CorpusScopes: map[string]bool{}, AffiliateHosts: map[string]string{},
		BaseRuleIDs: ruleIDs(b.srcs), HeadRuleIDs: ruleIDs(h.srcs),
		BaseDebounceGroups: debounceGroups(b.srcs), HeadDebounceGroups: debounceGroups(h.srcs)}

	items := readCorpusFS(t, tree)
	var hs, bs corpusStats
	if h.c != nil {
		hs = evalCorpus(h.c, items)
	}
	if b.c != nil {
		bs = evalCorpus(b.c, items)
	}
	for i, it := range items {
		ci := summaryCorpusItem{ID: it.ID, Input: it.Input, Expected: it.Expected, Alt: append([]string{}, it.Alt...)}
		if h.c != nil {
			ci.HeadOutput, ci.HeadBroken = hs.Results[i].Out, hs.Results[i].Broken
		}
		if b.c != nil {
			ci.BaseOutput, ci.BaseBroken = bs.Results[i].Out, bs.Results[i].Broken
		}
		sd.Corpus = append(sd.Corpus, ci)
	}

	var inputs []string
	for _, it := range items {
		inputs = append(inputs, it.Input)
	}
	scopes := map[string]bool{"global": true}
	for _, fsys := range []fs.FS{head, base} {
		if fsys == nil {
			continue
		}
		for _, p := range lockedPaths[:3] {
			data, _ := fs.ReadFile(fsys, p.vendored)
			for _, m := range lenientElems(data) {
				for _, inc := range lenientStrings(m["include"]) {
					scopes[inc] = true
				}
			}
		}
	}
	for _, s := range h.srcs {
		if s.Name == overlaySet {
			for _, r := range s.Rules {
				for _, inc := range r.Include {
					scopes[inc] = true
				}
			}
		}
	}
	for sc := range scopes {
		sd.CorpusScopes[sc] = sc == "global" && len(inputs) > 0 || slices.ContainsFunc(inputs, func(u string) bool { return patternMatches(sc, u) })
	}

	baseNotes := map[string]bool{}
	for _, n := range overlayHygiene(b.srcs) {
		baseNotes[n.key()] = true
	}
	for _, n := range overlayHygiene(h.srcs) {
		n.AtBase = baseNotes[n.key()]
		sd.Hygiene = append(sd.Hygiene, n)
	}

	if cls, err := fs.ReadFile(head, classesPath); err == nil {
		var doc struct {
			Patterns map[string]struct {
				Class   string `json:"class"`
				Network string `json:"network"`
			} `json:"patterns"`
		}
		if json.Unmarshal(cls, &doc) == nil {
			for p, c := range doc.Patterns {
				if c.Class != "affiliate" {
					continue
				}
				_, rest, _ := strings.Cut(p, "://")
				host, _, _ := strings.Cut(rest, "/")
				sd.AffiliateHosts[strings.TrimPrefix(host, "*.")] = c.Network
			}
		}
	}
	return sd
}

func ruleIDs(srcs []RuleSet) []string {
	out := []string{}
	for _, s := range srcs {
		for _, r := range s.Rules {
			out = append(out, s.Name+":"+r.ID)
		}
	}
	slices.Sort(out)
	return out
}

func debounceGroups(srcs []RuleSet) map[string][]string {
	out := map[string][]string{}
	for _, s := range srcs {
		if s.Name != "brave/debounce" {
			continue
		}
		for _, r := range s.Rules {
			out[s.Name+":"+r.ID] = slices.Clone(r.Include)
		}
	}
	return out
}

// patternMatches reports whether a match pattern matches a URL; a pattern or
// URL that does not parse matches nothing.
func patternMatches(pattern, raw string) bool {
	u, sk := rawurl.Parse(raw)
	if sk != rawurl.OK {
		return false
	}
	p, err := matchpattern.Parse(pattern, true)
	if err != nil {
		return false
	}
	tg := matchpattern.Target{Scheme: u.Scheme, Host: u.Host, IsIP: u.IsIP, Port: u.EffectivePort(), Site: rawurl.Site(u.Host)}
	tg.SetPath(u.PathForRequest())
	return p.Match(&tg)
}

func (n summaryHygiene) key() string {
	return n.Kind + "\x00" + n.Rule + "\x00" + n.Param + "\x00" + n.Scope + "\x00" + n.With
}

// overlayHygiene finds the §11.1 overlay hygiene notes, probing includes as
// keep coverage does (§11.3).
func overlayHygiene(srcs []RuleSet) []summaryHygiene {
	// overlaps: r applies somewhere in scope. covers: r applies at scope's
	// probe URL, i.e. throughout scope as far as a probe can tell.
	covers := func(r Rule, scope string) bool {
		return slices.ContainsFunc(r.Include, func(bi string) bool { return isGlobalPattern(bi) || patternMatches(bi, probeURL(scope)) })
	}
	overlaps := func(r Rule, scope string) bool {
		return covers(r, scope) || slices.ContainsFunc(r.Include, func(bi string) bool { return patternMatches(scope, probeURL(bi)) })
	}
	type braveRule struct {
		id string
		r  Rule
	}
	var strips, debounce []braveRule
	var overlay []Rule
	for _, s := range srcs {
		for _, r := range s.Rules {
			switch {
			case s.Name == overlaySet:
				overlay = append(overlay, r)
			case s.Name == "brave/debounce":
				debounce = append(debounce, braveRule{s.Name + ":" + r.ID, r})
			default:
				strips = append(strips, braveRule{s.Name + ":" + r.ID, r})
			}
		}
	}
	stripper := func(param, scope string, in func(Rule, string) bool) string {
		for _, b := range strips {
			if slices.ContainsFunc(b.r.Params, func(p string) bool { return query.Canon(p) == param }) && in(b.r, scope) {
				return b.id
			}
		}
		return ""
	}
	// A Keep of a name an overlay Strip also strips (an A-entry pair, or a
	// narrowing such as marketo-unsubscribe) overrides the overlay, not
	// upstream, so it is never a stale keep-override.
	overlayStrips := map[string]bool{}
	for _, o := range overlay {
		if o.Action == Strip {
			for _, p := range o.Params {
				overlayStrips[query.Canon(p)] = true
			}
		}
	}
	var out []summaryHygiene
	for _, o := range overlay {
		switch o.Action {
		case Keep:
			for _, p := range o.Params {
				cp := query.Canon(p)
				if !overlayStrips[cp] && !slices.ContainsFunc(o.Include, func(sc string) bool { return stripper(cp, sc, overlaps) != "" }) {
					out = append(out, summaryHygiene{Kind: "stale_keep", Rule: o.ID, Param: cp})
				}
			}
		case Strip:
			if o.Affiliate {
				continue // A-entry strips pair with their Keep on purpose
			}
			for _, p := range o.Params {
				cp := query.Canon(p)
				with := ""
				for _, sc := range o.Include {
					if with = stripper(cp, sc, covers); with == "" {
						break
					}
				}
				if with != "" {
					out = append(out, summaryHygiene{Kind: "duplicated_strip", Rule: o.ID, Param: cp, With: with})
				}
			}
		case Unwrap:
			for _, sc := range o.Include {
				for _, d := range debounce {
					if overlaps(d.r, sc) && !isGlobalPattern(sc) {
						out = append(out, summaryHygiene{Kind: "overlap", Rule: o.ID, Scope: sc, With: d.id})
					}
				}
			}
		}
	}
	return out
}

// TestSummaryDataContract builds the summary data for the real data against
// itself and for a WSJ st drop, and checks the fields summary relies on.
func TestSummaryDataContract(t *testing.T) {
	real, err := realInputs()
	if err != nil {
		t.Fatal(err)
	}
	head := mapFS(real, nil)
	sd := buildSummaryData(t, head, head, head)
	if sd.Schema != 1 || sd.BaseError != "" || sd.HeadError != "" {
		t.Fatalf("schema %d, errors %q / %q", sd.Schema, sd.BaseError, sd.HeadError)
	}
	if len(sd.Corpus) != len(readCorpus(t)) {
		t.Errorf("%d corpus items", len(sd.Corpus))
	}
	for _, c := range sd.Corpus {
		if c.BaseOutput != c.HeadOutput || c.BaseBroken != c.HeadBroken || c.HeadOutput == "" {
			t.Errorf("%s: base %q (%v), head %q (%v)", c.ID, c.BaseOutput, c.BaseBroken, c.HeadOutput, c.HeadBroken)
		}
	}
	if !slices.Equal(sd.BaseRuleIDs, sd.HeadRuleIDs) || !slices.Contains(sd.HeadRuleIDs, "brave/debounce:*://www.clixgalore.com/Lead.aspx*#2") {
		t.Errorf("rule IDs: %d base, %d head", len(sd.BaseRuleIDs), len(sd.HeadRuleIDs))
	}
	if g := sd.HeadDebounceGroups["brave/debounce:*://www.clixgalore.com/Lead.aspx*"]; !slices.Equal(g, []string{"*://www.clixgalore.com/Lead.aspx*"}) {
		t.Errorf("clixGalore group %q", g)
	}
	if !sd.CorpusScopes["*://www.wsj.com/*"] || !sd.CorpusScopes["global"] || sd.CorpusScopes["*://*.dttq.net/c/*"] {
		t.Errorf("corpus scopes: wsj %v, global %v, dttq %v", sd.CorpusScopes["*://www.wsj.com/*"], sd.CorpusScopes["global"], sd.CorpusScopes["*://*.dttq.net/c/*"])
	}
	if sd.AffiliateHosts["3anx.net"] != "Impact" || sd.AffiliateHosts["www.clixgalore.com"] != "clixGalore" {
		t.Errorf("affiliate hosts: 3anx.net %q, clixgalore %q", sd.AffiliateHosts["3anx.net"], sd.AffiliateHosts["www.clixgalore.com"])
	}
	overlap := slices.ContainsFunc(sd.Hygiene, func(n summaryHygiene) bool {
		return n.Kind == "overlap" && n.Rule == "squarespace-mail" && n.AtBase
	})
	if !overlap {
		t.Errorf("no squarespace-mail overlap note in %+v", sd.Hygiene)
	}
	for _, n := range sd.Hygiene {
		if n.Kind == "stale_keep" && n.Rule == "wsj-gift-st" {
			t.Errorf("wsj-gift-st is stale on current data: %+v", n)
		}
	}

	// keep_override_stale: upstream drops WSJ st.
	s := &fixtureState{t: t, file: "contract", head: cloneFiles(real, nil)}
	s.op("contract", `jremove rules/brave/clean-urls.json [{"include": "*://www.wsj.com/*"}, "params"] "st"`)
	s.op("contract", "relock")
	sd = buildSummaryData(t, mapFS(s.head, nil), head, head)
	stale := slices.ContainsFunc(sd.Hygiene, func(n summaryHygiene) bool {
		return n.Kind == "stale_keep" && n.Rule == "wsj-gift-st" && n.Param == "st" && !n.AtBase
	})
	if !stale {
		t.Errorf("no new stale_keep note for wsj-gift-st: %+v", sd.Hygiene)
	}

	// Upstream now strips the overlay's nyt-smtyp name on all of nytimes.com.
	s.op("contract", `jappend rules/brave/clean-urls.json [] {"include": ["*://*.nytimes.com/*"], "exclude": [], "params": ["smtyp"]}`)
	s.op("contract", "relock")
	sd = buildSummaryData(t, mapFS(s.head, nil), head, head)
	dup := slices.ContainsFunc(sd.Hygiene, func(n summaryHygiene) bool {
		return n.Kind == "duplicated_strip" && n.Rule == "nyt-smtyp" && strings.HasPrefix(n.With, "brave/clean-urls:*://*.nytimes.com/*") && !n.AtBase
	})
	if !dup {
		t.Errorf("no duplicated_strip note for nyt-smtyp: %+v", sd.Hygiene)
	}

	// A side that does not compile reports its error and empty outputs.
	s.op("contract", `jset rules/brave/debounce.json [0, "action"] "rot13,redirect"`)
	s.op("contract", "relock")
	sd = buildSummaryData(t, mapFS(s.head, nil), nil, head)
	if !strings.Contains(sd.HeadError, "rot13,redirect") || sd.BaseError != "no base" || sd.Corpus[0].HeadOutput != "" {
		t.Errorf("errors %q / %q, head output %q", sd.HeadError, sd.BaseError, sd.Corpus[0].HeadOutput)
	}
	if _, err := json.Marshal(sd); err != nil {
		t.Error(err)
	}
	t.Logf("hygiene notes on current data: %d", len(buildSummaryData(t, head, head, head).Hygiene))
}
