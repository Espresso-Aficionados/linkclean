package linkclean

// Clean cleans rawURL with Default().
func Clean(rawURL string) string { return Default().Clean(rawURL) }

// Default returns the shared default Cleaner: DefaultSources() with the zero
// Config (AffiliateKeep, unwrap on, nested on, signed and archive guards on).
// It is compiled once, on first call, and is safe for concurrent use. It panics
// only if the embedded data fails to compile; the release gates make that
// unreachable for tagged versions.
func Default() *Cleaner {
	panic("TODO(INT): Default")
}

// New validates and compiles cfg. Inputs are deep-copied: later mutation by the
// caller has no effect, and no caller slice is modified. Rule problems are
// returned together (errors.Join of *RuleError); New never silently drops an
// invalid rule. Unmatched DisableRules/DisableRulePrefixes/DisableTags entries
// are reported by Cleaner.Warnings, or as errors when cfg.StrictDisable is set.
// Build once: New takes about a millisecond for the defaults. With
// cfg.Sources == nil the compiled built-in rules are shared by every Cleaner in
// the process, so the per-Cleaner cost is O(len(Extra) + knobs).
func New(cfg Config) (*Cleaner, error) {
	panic("TODO(INT): New")
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
	// TODO(INT): *engine.Cleaner, []SourceInfo, notices, []Warning.
}

// Clean returns the cleaned URL. It is the fast path: no Result is built, and an
// unchanged input allocates nothing.
func (c *Cleaner) Clean(rawURL string) string {
	panic("TODO(INT): Cleaner.Clean")
}

// Explain cleans rawURL and reports what was done and why.
// Explain(s).URL == Clean(s) for every s.
func (c *Cleaner) Explain(rawURL string) Result {
	panic("TODO(INT): Cleaner.Explain")
}

// Sources reports the compiled rule sets (name, license, upstream, commit,
// sha256, counts) in priority order. Log it at startup so bug reports name the
// data version.
func (c *Cleaner) Sources() []SourceInfo {
	panic("TODO(INT): Cleaner.Sources")
}

// Notices returns the attribution and license notices of every compiled source
// (MPL-2.0 §3.2 "where to get the source"), for --licenses output or an about page.
func (c *Cleaner) Notices() string {
	panic("TODO(INT): Cleaner.Notices")
}

// Warnings returns non-fatal configuration problems found by New, such as a
// DisableRules entry that matches no rule after a data refresh. Callers should
// log them at startup. The slice is a copy.
func (c *Cleaner) Warnings() []Warning {
	panic("TODO(INT): Cleaner.Warnings")
}
