package linkclean

// Config configures a Cleaner. The zero value is the recommended default, and
// every field is safe at its zero value. Future fields follow the same rule.
type Config struct {
	// Sources are the rule sets to compile. Nil means DefaultSources(). A non-nil
	// empty slice means no built-in sources (only Extra applies).
	Sources []RuleSet

	// Extra holds the caller's own rules (rule-set name "user"). They have the
	// highest priority: their Unwrap rules are tried first and their removals are
	// attributed first. Keep rules win regardless of priority.
	Extra []Rule

	// Affiliate decides what happens to affiliate parameters and affiliate
	// unwrappers. Zero value: AffiliateKeep.
	Affiliate AffiliatePolicy

	// DisableRules lists full rule IDs ("<set>:<id>") to skip. Matching is EXACT
	// and byte-for-byte; '*' has no special meaning (most Brave IDs end in '*',
	// e.g. "brave/clean-urls:*://*.amazon.ae/*"). An entry that matches no rule
	// produces a Warning (ErrUnknownRule), or an error from New when
	// StrictDisable is set.
	DisableRules []string

	// DisableRulePrefixes skips every rule whose full ID starts with one of these
	// strings (e.g. "linkclean/overlay:amazon-"). Each entry must contain the
	// "<set>:" part. An entry that matches no rule is reported like DisableRules.
	DisableRulePrefixes []string

	// DisableTags skips every rule carrying one of these tags (e.g. "fragment",
	// "unwrap", "debounce", "lowconf"). Unknown tags are reported like
	// DisableRules.
	DisableTags []string

	// StrictDisable makes unmatched DisableRules/DisableRulePrefixes/DisableTags
	// entries an error from New instead of a Warning.
	StrictDisable bool

	// DisableUnwrap turns off offline redirect-wrapper unwrapping.
	DisableUnwrap bool

	// MaxUnwrap bounds unwrap hops. 0 means 5; values above 10 are clamped to 10.
	// If no fixed point is reached within the bound, or a cycle is seen, unwrapping
	// is abandoned and the original input is cleaned (Result.UnwrapAbandoned).
	MaxUnwrap int

	// DisableNested turns off depth-1 cleaning of absolute http(s) URLs embedded
	// in query values.
	DisableNested bool

	// DisableSignedGuard turns off the signed-URL guard. With the guard on, a URL
	// whose query carries a signature parameter gets no nested edits and no
	// global-scope strips; host-scoped rules still apply.
	DisableSignedGuard bool

	// ExtraSignedParams extends the built-in signature parameter list.
	// Names are compared canonically.
	ExtraSignedParams []string

	// DisableArchiveGuard turns off the archive guard, which returns
	// archive-replay and security-rewriter URLs unchanged.
	DisableArchiveGuard bool

	// ExtraArchivePatterns extends the archive guard with match patterns (the
	// Include grammar), e.g. "*://archive.example.org/replay/*".
	ExtraArchivePatterns []string

	// MaxURLLength: 0 means 8192 bytes. Longer input is returned unchanged.
	// Work per URL is linear in its length; an inline proxy cleaning untrusted
	// input should set 2048.
	MaxURLLength int
}

// AffiliatePolicy decides whether affiliate attribution is kept or removed.
type AffiliatePolicy uint8

const (
	AffiliateKeep  AffiliatePolicy = iota // zero value: affiliate params and affiliate unwrappers are left alone
	AffiliateStrip                        // affiliate params are stripped and affiliate Unwrap rules apply
)
