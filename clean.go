package linkclean

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Espresso-Aficionados/linkclean/internal/engine"
	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

const (
	defaultMaxUnwrap    = 5
	maxMaxUnwrap        = 10
	defaultMaxURLLength = 8192
	userSet             = "user" // rule-set name of Config.Extra
)

// Clean cleans rawURL with Default().
func Clean(rawURL string) string { return Default().Clean(rawURL) }

// Default returns the shared default Cleaner: DefaultSources() with the zero
// Config (AffiliateKeep, unwrap on, nested on, signed and archive guards on).
// It is compiled once, on first call, and is safe for concurrent use. It panics
// only if the embedded data fails to compile; the release gates make that
// unreachable for tagged versions.
func Default() *Cleaner { return defaultCleaner() }

var defaultCleaner = sync.OnceValue(func() *Cleaner { return MustNew(Config{}) })

// defaultBase is the built-in rule sets compiled once per process and shared
// by every Cleaner whose Config.Sources is nil (§8).
var defaultBase = sync.OnceValues(func() (*engine.Program, error) {
	sets, err := decodeDefaults()
	if err != nil {
		return nil, err
	}
	in := make([]rule.Set, len(sets))
	for i := range sets {
		in[i] = sets[i].toInternal()
	}
	return engine.Compile(in)
})

// New validates and compiles cfg. Inputs are deep-copied: later mutation by the
// caller has no effect, and no caller slice is modified. Rule problems are
// returned together (errors.Join of *RuleError); New never silently drops an
// invalid rule. Unmatched DisableRules/DisableRulePrefixes/DisableTags entries
// are reported by Cleaner.Warnings, or as errors when cfg.StrictDisable is set.
// Build once: New takes about a millisecond for the defaults. With
// cfg.Sources == nil the compiled built-in rules are shared by every Cleaner in
// the process, so the per-Cleaner cost is O(len(Extra) + knobs).
func New(cfg Config) (*Cleaner, error) {
	var errs []error
	cfgErr := func(field string, err error) {
		errs = append(errs, &RuleError{Field: field, Err: err})
	}
	opt := engine.Options{
		AffiliateStrip:      cfg.Affiliate == AffiliateStrip,
		DisableRules:        slices.Clone(cfg.DisableRules),
		DisableRulePrefixes: slices.Clone(cfg.DisableRulePrefixes),
		DisableTags:         slices.Clone(cfg.DisableTags),
		DisableUnwrap:       cfg.DisableUnwrap,
		DisableNested:       cfg.DisableNested,
		DisableSignedGuard:  cfg.DisableSignedGuard,
		DisableArchiveGuard: cfg.DisableArchiveGuard,
		MaxUnwrap:           cfg.MaxUnwrap,
		MaxURLLength:        cfg.MaxURLLength,
		ExtraSignedParams:   slices.Clone(cfg.ExtraSignedParams),
	}
	if cfg.Affiliate > AffiliateStrip {
		cfgErr("Affiliate", fmt.Errorf("invalid policy %d", cfg.Affiliate))
	}
	switch {
	case cfg.MaxUnwrap < 0:
		cfgErr("MaxUnwrap", fmt.Errorf("negative: %d", cfg.MaxUnwrap))
	case cfg.MaxUnwrap == 0:
		opt.MaxUnwrap = defaultMaxUnwrap
	case cfg.MaxUnwrap > maxMaxUnwrap:
		opt.MaxUnwrap = maxMaxUnwrap
	}
	switch {
	case cfg.MaxURLLength < 0:
		cfgErr("MaxURLLength", fmt.Errorf("negative: %d", cfg.MaxURLLength))
	case cfg.MaxURLLength == 0:
		opt.MaxURLLength = defaultMaxURLLength
	}
	for i, s := range cfg.ExtraSignedParams {
		if s == "" {
			cfgErr(fmt.Sprintf("ExtraSignedParams[%d]", i), errors.New("empty name"))
		}
	}
	for i, s := range cfg.ExtraArchivePatterns {
		p, err := matchpattern.Parse(s, true)
		if err != nil {
			cfgErr(fmt.Sprintf("ExtraArchivePatterns[%d]", i), err)
			continue
		}
		opt.ExtraArchive = append(opt.ExtraArchive, p)
	}
	for i, s := range cfg.DisableRulePrefixes {
		if !strings.Contains(s, ":") {
			cfgErr(fmt.Sprintf("DisableRulePrefixes[%d]", i), fmt.Errorf("%q has no \"<set>:\" part", s))
		}
	}

	var progs []*engine.Program
	if len(cfg.Extra) > 0 {
		user := rule.Set{Name: userSet, Rules: make([]rule.Rule, len(cfg.Extra))}
		for i := range cfg.Extra {
			user.Rules[i] = cfg.Extra[i].toInternal()
		}
		p, err := engine.Compile([]rule.Set{user})
		errs = append(errs, convertErrs(err)...)
		progs = append(progs, p)
	}
	if cfg.Sources == nil {
		p, err := defaultBase()
		errs = append(errs, convertErrs(err)...)
		progs = append(progs, p)
	} else {
		names := map[string]bool{userSet: true}
		sets := make([]rule.Set, len(cfg.Sources))
		for i := range cfg.Sources {
			s := &cfg.Sources[i]
			switch {
			case s.Name == "":
				cfgErr(fmt.Sprintf("Sources[%d].Name", i), errors.New("empty rule-set name"))
			case names[s.Name]:
				errs = append(errs, &RuleError{Set: s.Name, Field: fmt.Sprintf("Sources[%d].Name", i),
					Err: errors.New(`duplicate or reserved rule-set name ("user" is Config.Extra)`)})
			}
			names[s.Name] = true
			sets[i] = s.toInternal()
		}
		p, err := engine.Compile(sets)
		errs = append(errs, convertErrs(err)...)
		progs = append(progs, p)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	eng, unmatched := engine.NewCleaner(progs, opt)
	c := &Cleaner{eng: eng}
	for _, u := range unmatched {
		w := Warning{Field: u.Field, Err: ErrUnknownRule}
		switch u.Field {
		case "DisableRules":
			w.Entry = cfg.DisableRules[u.Index]
		case "DisableRulePrefixes":
			w.Entry = cfg.DisableRulePrefixes[u.Index]
		case "DisableTags":
			w.Entry, w.Err = cfg.DisableTags[u.Index], ErrUnknownTag
		}
		if cfg.StrictDisable {
			errs = append(errs, &RuleError{Field: fmt.Sprintf("%s[%d]", u.Field, u.Index), Err: fmt.Errorf("%w: %q", w.Err, w.Entry)})
			continue
		}
		c.warnings = append(c.warnings, w)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	var notices []string
	for _, p := range progs {
		for _, s := range p.Sets() {
			c.sources = append(c.sources, SourceInfo{
				Name: s.Name, License: s.License, Upstream: s.Upstream, Commit: s.Commit, SHA256: s.SHA256,
				Rules: s.Rules, Params: s.Params, Regexps: s.Regexps,
			})
			if s.Notice != "" {
				notices = append(notices, s.Notice)
			}
		}
	}
	c.notices = strings.Join(notices, "\n\n")
	return c, nil
}

// convertErrs turns the *rule.Error values in a joined error into *RuleError.
func convertErrs(err error) []error {
	if err == nil {
		return nil
	}
	var in []error
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		in = j.Unwrap()
	} else {
		in = []error{err}
	}
	out := make([]error, 0, len(in))
	for _, e := range in {
		if re, ok := e.(*rule.Error); ok {
			e = &RuleError{Set: re.Set, RuleID: re.RuleID, Field: re.Field, Err: re.Err}
		}
		out = append(out, e)
	}
	return out
}

// MustNew is like New but panics on error. Warnings never panic.
func MustNew(cfg Config) *Cleaner {
	c, err := New(cfg)
	if err != nil {
		panic(err)
	}
	return c
}

// Cleaner is a compiled, immutable rule set, safe for concurrent use. It holds
// no caches, locks or lazily-initialized state.
type Cleaner struct {
	eng      *engine.Cleaner
	sources  []SourceInfo
	notices  string
	warnings []Warning
}

// Clean returns the cleaned URL. It is the fast path: no Result is built, and an
// unchanged input allocates nothing.
func (c *Cleaner) Clean(rawURL string) string { return c.eng.Clean(rawURL) }

// Explain cleans rawURL and reports what was done and why.
// Explain(s).URL == Clean(s) for every s.
func (c *Cleaner) Explain(rawURL string) Result {
	r := c.eng.Explain(rawURL)
	res := Result{
		Input:           rawURL,
		URL:             r.URL,
		Changed:         r.URL != rawURL,
		Skipped:         SkipReason(r.Skipped),
		UnwrapAbandoned: r.UnwrapAbandoned,
		Signed:          r.Signed,
	}
	for _, h := range r.Hops {
		res.Hops = append(res.Hops, Hop{
			From: h.From, To: h.To, Rule: h.Rule, Category: Category(h.Category), FromHost: h.FromHost,
			Beneficiary: Beneficiary(h.Beneficiary),
		})
	}
	for _, m := range r.Removed {
		res.Removed = append(res.Removed, Removal{
			Part: Part(m.Part), Category: Category(m.Category),
			Name: m.Name, Value: m.Value, Text: m.Text, NestedIn: m.NestedIn, Rule: m.Rule,
			Beneficiary: Beneficiary(m.Beneficiary),
		})
	}
	for _, k := range r.Kept {
		res.Kept = append(res.Kept, Kept{Name: k.Name, Reason: KeepReason(k.Reason), Rule: k.Rule})
	}
	return res
}

// Sources reports the compiled rule sets (name, license, upstream, commit,
// sha256, counts) in priority order, Config.Extra first as "user". Log it at
// startup so bug reports name the data version.
func (c *Cleaner) Sources() []SourceInfo { return slices.Clone(c.sources) }

// Notices returns the attribution and license notices of every compiled source
// (MPL-2.0 §3.2 "where to get the source"), for --licenses output or an about page.
func (c *Cleaner) Notices() string { return c.notices }

// Warnings returns non-fatal configuration problems found by New, such as a
// DisableRules entry that matches no rule after a data refresh. Callers should
// log them at startup. The slice is a copy.
func (c *Cleaner) Warnings() []Warning { return slices.Clone(c.warnings) }
