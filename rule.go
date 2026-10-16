package linkclean

import (
	"fmt"
	"slices"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// RuleSet is a named, licensed collection of rules. Values are plain data.
type RuleSet struct {
	Name     string `json:"name"`     // "brave/clean-urls", "brave/query-filter", "brave/debounce", "linkclean/overlay"; "user" is reserved
	License  string `json:"license"`  // SPDX id of the data, e.g. "MPL-2.0"
	Upstream string `json:"upstream"` // e.g. "github.com/brave/adblock-lists brave-lists/clean-urls.json"
	Commit   string `json:"commit"`   // upstream commit; "" for hand-authored sets
	SHA256   string `json:"sha256"`   // of the embedded upstream bytes; "" for hand-authored sets
	Notice   string `json:"notice"`   // attribution text returned by Cleaner.Notices
	Rules    []Rule `json:"rules"`
}

// Action is what a rule does to an in-scope URL.
type Action uint8

const (
	Strip  Action = iota + 1 // remove matching params (and PathRegexps text) on in-scope URLs
	Keep                     // protect matching params from EVERY Strip rule, from any source
	Unwrap                   // replace the whole URL with an embedded destination
)

var actionNames = [...]string{Strip: "strip", Keep: "keep", Unwrap: "unwrap"}

// MarshalText returns "strip", "keep" or "unwrap".
func (a Action) MarshalText() ([]byte, error) {
	if a == 0 || int(a) >= len(actionNames) {
		return nil, fmt.Errorf("linkclean: invalid Action %d", uint8(a))
	}
	return []byte(actionNames[a]), nil
}

// UnmarshalText accepts "strip", "keep" or "unwrap".
func (a *Action) UnmarshalText(b []byte) error {
	for i, n := range actionNames {
		if i > 0 && n == string(b) {
			*a = Action(i)
			return nil
		}
	}
	return fmt.Errorf("linkclean: unknown action %q", b)
}

// Rule is one normalized rule. JSON tags are part of the API: consumers may
// json.Unmarshal a []Rule into Config.Extra. New validates everything and rejects
// fields that make no sense for the Action. New fields added after v1 are
// optional and zero-valued, so v1 rule JSON stays valid.
type Rule struct {
	// ID is unique within its RuleSet. The full ID used in Results, Warnings and
	// DisableRules is "<RuleSet.Name>:<ID>".
	ID     string `json:"id"`
	Action Action `json:"action"`

	// Affiliate marks an affiliate rule:
	//   Strip+Affiliate and Unwrap+Affiliate apply only under AffiliateStrip;
	//   Keep+Affiliate applies only under AffiliateKeep.
	// Built-in Unwrap+Affiliate rules are the debounce patterns classified
	// "affiliate". New rejects Affiliate together with PathRegexps.
	Affiliate bool `json:"affiliate,omitempty"`

	// Network names the affiliate network or programme ("Amazon Associates").
	// Affiliate rules only; required on built-in affiliate rules, optional in
	// Extra. Copied into Beneficiary.Network. New rejects a value outside
	// ^[A-Za-z0-9][A-Za-z0-9 .&'()/-]{0,39}$, so it is safe to display verbatim.
	Network string `json:"network,omitempty"`

	// IDParam names the ONE parameter whose value identifies the beneficiary
	// (publisher, creator, referrer). Affiliate rules only. On Strip/Keep it must
	// canon-equal one of Params; on Unwrap it is read from the wrapper's query.
	// Empty means the network has no single beneficiary ID.
	IDParam string `json:"idParam,omitempty"`

	// IDPathRegexp (Unwrap+Affiliate only; exclusive with IDParam): RE2 over the
	// wrapper's raw path with exactly one capture group, the beneficiary ID
	// (CJ "^/click-([0-9]+)-", Impact "^/c/([0-9]+)/").
	IDPathRegexp string `json:"idPathRegexp,omitempty"`

	// Tags label rules for Config.DisableTags.
	Tags []string `json:"tags,omitempty"`

	// Ref is free-form provenance (an evidence URL, corpus item id, or vendor doc).
	// The engine ignores it. Overlay entries must set it.
	Ref string `json:"ref,omitempty"`

	// Scope: the rule applies iff some Include pattern matches, no Exclude
	// pattern matches, and (URLRegexps is empty or some URLRegexps entry matches).
	// At least one Include is required, so every rule is host-scoped by
	// construction ("*://*/*" is the explicit global scope). Pattern hosts
	// are canonicalized like URL hosts: lowercased, and a Unicode host is
	// mapped to punycode by IDNA (New rejects one that IDNA refuses).
	Include    []string `json:"include"`              // match patterns
	Exclude    []string `json:"exclude,omitempty"`    // same grammar; exempts THIS rule only
	URLRegexps []string `json:"urlRegexps,omitempty"` // RE2, unanchored search over the match string

	// Params are exact names, compared as canon(name): %XX decoded once, then
	// ASCII-lowercased (Strip, Keep).
	Params []string `json:"params,omitempty"`
	// ParamRegexps are RE2 compiled as ^(?:re)$ over canon(name). Built-in
	// global-scope ParamRegexps are probed against the never-strip list at build
	// time. Extra rules are NOT probed: a global ParamRegexps in Extra is the
	// caller's responsibility.
	ParamRegexps []string `json:"paramRegexps,omitempty"`
	// InFragment also applies Params/ParamRegexps to a param-shaped fragment,
	// all-or-nothing.
	InFragment bool `json:"inFragment,omitempty"`

	// PathRegexps (Strip only): every match inside the raw PATH span is deleted.
	PathRegexps []string `json:"pathRegexps,omitempty"`

	// Unwrap (Action == Unwrap only).
	Unwrap *UnwrapSpec `json:"unwrap,omitempty"`
}

// UnwrapSpec extracts an embedded destination URL. QueryParams is required.
// Extracted values are percent-decoded once ("%XX" only; '+' stays literal),
// validated and re-serialized.
type UnwrapSpec struct {
	// QueryParams are tried in order; for each, the first query segment whose
	// canon(name) matches. The first value that yields a VALID target wins
	// (handles Google /url's empty q= before url=).
	QueryParams []string `json:"queryParams,omitempty"`
	// AllowSameSite permits a destination with the same ICANN eTLD+1 as the
	// wrapper. Default false (Brave's safeguard).
	AllowSameSite bool `json:"allowSameSite,omitempty"`
	// PrependScheme ("http" or "https"): if the decoded value does not start
	// with "http://" or "https://" (ASCII case-insensitive), this scheme and
	// "://" are prepended before validation. A value that already has a scheme
	// is used as is.
	PrependScheme string `json:"prependScheme,omitempty"`
}

// toInternal deep-copies r into the internal rule model.
func (r *Rule) toInternal() rule.Rule {
	out := rule.Rule{
		ID:           r.ID,
		Action:       rule.Action(r.Action),
		Affiliate:    r.Affiliate,
		Network:      r.Network,
		IDParam:      r.IDParam,
		IDPathRegexp: r.IDPathRegexp,
		Tags:         slices.Clone(r.Tags),
		Ref:          r.Ref,
		Include:      slices.Clone(r.Include),
		Exclude:      slices.Clone(r.Exclude),
		URLRegexps:   slices.Clone(r.URLRegexps),
		Params:       slices.Clone(r.Params),
		ParamRegexps: slices.Clone(r.ParamRegexps),
		InFragment:   r.InFragment,
		PathRegexps:  slices.Clone(r.PathRegexps),
	}
	if u := r.Unwrap; u != nil {
		out.Unwrap = &rule.UnwrapSpec{
			QueryParams:   slices.Clone(u.QueryParams),
			AllowSameSite: u.AllowSameSite,
			PrependScheme: u.PrependScheme,
		}
	}
	return out
}

// ruleFromInternal deep-copies r into the public rule type.
func ruleFromInternal(r *rule.Rule) Rule {
	out := Rule{
		ID:           r.ID,
		Action:       Action(r.Action),
		Affiliate:    r.Affiliate,
		Network:      r.Network,
		IDParam:      r.IDParam,
		IDPathRegexp: r.IDPathRegexp,
		Tags:         slices.Clone(r.Tags),
		Ref:          r.Ref,
		Include:      slices.Clone(r.Include),
		Exclude:      slices.Clone(r.Exclude),
		URLRegexps:   slices.Clone(r.URLRegexps),
		Params:       slices.Clone(r.Params),
		ParamRegexps: slices.Clone(r.ParamRegexps),
		InFragment:   r.InFragment,
		PathRegexps:  slices.Clone(r.PathRegexps),
	}
	if u := r.Unwrap; u != nil {
		out.Unwrap = &UnwrapSpec{
			QueryParams:   slices.Clone(u.QueryParams),
			AllowSameSite: u.AllowSameSite,
			PrependScheme: u.PrependScheme,
		}
	}
	return out
}

// toInternal deep-copies s into the internal rule model.
func (s *RuleSet) toInternal() rule.Set {
	out := rule.Set{
		Name: s.Name, License: s.License, Upstream: s.Upstream,
		Commit: s.Commit, SHA256: s.SHA256, Notice: s.Notice,
		Rules: make([]rule.Rule, len(s.Rules)),
	}
	for i := range s.Rules {
		out.Rules[i] = s.Rules[i].toInternal()
	}
	return out
}

// setFromInternal deep-copies s into the public type.
func setFromInternal(s *rule.Set) RuleSet {
	out := RuleSet{
		Name: s.Name, License: s.License, Upstream: s.Upstream,
		Commit: s.Commit, SHA256: s.SHA256, Notice: s.Notice,
		Rules: make([]Rule, len(s.Rules)),
	}
	for i := range s.Rules {
		out.Rules[i] = ruleFromInternal(&s.Rules[i])
	}
	return out
}

// cloneSet deep-copies s.
func cloneSet(s *RuleSet) RuleSet {
	in := s.toInternal()
	return setFromInternal(&in)
}
