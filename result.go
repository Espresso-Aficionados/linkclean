package linkclean

import "fmt"

// Result explains one Clean.
type Result struct {
	Input   string
	URL     string // == Clean(Input); == Input when !Changed
	Changed bool
	// Skipped is non-zero ⇒ URL == Input and there are no Hops, Removed or
	// Kept. Except for SkipRoundCap, nothing else was evaluated.
	Skipped         SkipReason
	Hops            []Hop     // unwrap hops in order; nil if none or abandoned
	UnwrapAbandoned bool      // cap or cycle; the original input was cleaned instead
	Signed          bool      // the signed-URL guard was active for the final URL
	Removed         []Removal // per strip round, in round order; within a round: path, query (input order), fragment, nested
	Kept            []Kept    // matched a strip rule but retained, and why
}

// Hop is one unwrap step. Rule is the full rule ID for offline hops. For
// network hops added by package unshorten, Category is CatUnshortened and Rule
// is "unshorten:<entry>" (an opaque label, not a rule ID).
type Hop struct {
	From, To string
	Rule     string
	// Category is CatRedirectUnwrapped (offline Unwrap rule, Affiliate false),
	// CatAffiliate (offline Unwrap+Affiliate rule; only under AffiliateStrip)
	// or CatUnshortened (network hop, package unshorten).
	Category Category
	// FromHost is the host view of From: lowercase ASCII, punycode, trailing
	// dots trimmed, no port, no userinfo. For CatUnshortened it is the
	// short-link host; otherwise the wrapper host.
	FromHost    string
	Beneficiary Beneficiary // CatAffiliate only; zero otherwise
}

// Category is the display category of one reported change. Every Removal and
// Hop has exactly one non-zero Category. MarshalText/UnmarshalText use
// "tracker", "affiliate", "path", "redirect", "unshortened". Constants may be
// added in minor releases; treat an unknown value as a generic change.
type Category uint8

const (
	CatTracker           Category = iota + 1 // Removal: query/fragment/nested param stripped as tracking
	CatAffiliate                             // Removal or Hop: stripped/unwrapped because an Affiliate rule applied (AffiliateStrip only)
	CatPathSegment                           // Removal: path text removed (PartPath)
	CatRedirectUnwrapped                     // Hop: offline unwrap of a redirect wrapper
	CatUnshortened                           // Hop: network hop added by package unshorten
)

var categoryNames = [...]string{
	CatTracker:           "tracker",
	CatAffiliate:         "affiliate",
	CatPathSegment:       "path",
	CatRedirectUnwrapped: "redirect",
	CatUnshortened:       "unshortened",
}

// MarshalText returns the category's name.
func (c Category) MarshalText() ([]byte, error) {
	if c == 0 || int(c) >= len(categoryNames) {
		return nil, fmt.Errorf("linkclean: invalid Category %d", uint8(c))
	}
	return []byte(categoryNames[c]), nil
}

// UnmarshalText accepts a name returned by MarshalText.
func (c *Category) UnmarshalText(b []byte) error {
	for i, n := range categoryNames {
		if i > 0 && n == string(b) {
			*c = Category(i)
			return nil
		}
	}
	return fmt.Errorf("linkclean: unknown category %q", b)
}

// Beneficiary names who is paid by an affiliate link. Set only on CatAffiliate
// Removals and Hops.
type Beneficiary struct {
	// Network is the matching rule's Network ("" if an Extra rule names none).
	Network string
	// ID is the beneficiary/publisher ID: the value of the rule's IDParam (or the
	// IDPathRegexp group), percent-decoded once ("%XX" only, '+' literal)
	// relative to the URL the segment sits in. For a PartNested removal that URL
	// is the nested value after its own single decode, so relative to the posted
	// text the ID has been decoded twice. Compare ID with Removal.Value only
	// after decoding Value once, never with the outer message bytes.
	// On a Removal it is set only on the removal OF the IDParam itself; the
	// network's other params (Amazon linkCode, linkId, ascsubtag) have ID "".
	// "" also when the value is empty or the rule has no locator.
	// ID IS ATTACKER-CONTROLLED TEXT: it may contain any bytes, including
	// mentions, markdown, bidi controls and invalid UTF-8. Validate before
	// display.
	ID string
}

// Part is the part of the URL a Removal came from.
type Part uint8

const (
	PartPath Part = iota + 1
	PartQuery
	PartFragment
	PartNested // inside a URL-valued query param (depth 1)
)

// Removal describes one removed piece. Name and Value split Text at its first
// '=': Text == Name + "=" + Value when Text contains '=', else Text == Name and
// Value == "". For query, fragment and nested removals Name is the raw param
// name as written (e.g. "utm_source", "%75tm_source"). For path removals it is
// the removed path text up to its first '=' ("/ref" for "/ref=sr_1_3"), or the
// whole removed text when there is no '='.
// Value and Text may contain a tracking identifier: never log or echo them.
// Consumers that show what was removed use Name (and, for CatAffiliate,
// Beneficiary.ID), never Value or Text.
type Removal struct {
	Part        Part
	Category    Category // CatTracker, CatAffiliate or CatPathSegment
	Name        string
	Value       string
	Text        string
	NestedIn    string      // raw outer param name for PartNested, else ""
	Rule        string      // full ID of the rule that decided Category
	Beneficiary Beneficiary // CatAffiliate only; zero otherwise
}

// KeepReason says why a parameter that matched a strip rule was retained.
type KeepReason uint8

const (
	KeptByRule      KeepReason = iota + 1 // a non-affiliate Keep rule (overlay or user)
	KeptAffiliate                         // an affiliate Keep rule under AffiliateKeep
	KeptSignedGuard                       // only global-scope strips matched, and the URL is signed
)

// Kept is one parameter that matched a strip rule but was retained.
type Kept struct {
	Name   string
	Reason KeepReason
	Rule   string // the Keep rule responsible ("" for KeptSignedGuard)
}

// SkipReason says why an input was returned unchanged without cleaning.
type SkipReason uint8

const (
	NotSkipped      SkipReason = iota
	SkipScheme                 // not http/https
	SkipUnparseable            // no "://", empty host
	SkipAmbiguous              // byte <= 0x20 or 0x7F anywhere; '\' in authority; '%' in host; bad brackets
	SkipTooLong                // longer than Config.MaxURLLength
	SkipArchive                // archive-replay or security-rewriter URL
	SkipRoundCap               // no fixed point within 8 strip rounds; reachable only with Extra or custom Sources
)

// SourceInfo describes one compiled rule set.
type SourceInfo struct {
	Name, License, Upstream, Commit, SHA256 string
	Rules, Params, Regexps                  int
}
