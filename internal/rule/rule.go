// Package rule is the normalized rule model shared by the compilers
// (internal/brave), the engine (internal/engine) and the public API.
//
// It mirrors linkclean.Rule and linkclean.RuleSet field for field. The public
// types are the frozen API; these are the internal contract, so the root
// package can import the compilers without an import cycle. The root package
// converts between the two (a deep copy, which New and DefaultSources need
// anyway). Values here are plain data: no validation, no compiled state.
package rule

import "fmt"

// Action is what a rule does to an in-scope URL. The values equal
// linkclean.Action's.
type Action uint8

const (
	Strip  Action = iota + 1 // remove matching params (and PathRegexps text)
	Keep                     // protect matching params from every Strip rule
	Unwrap                   // replace the URL with an embedded destination
)

func (a Action) String() string {
	switch a {
	case Strip:
		return "strip"
	case Keep:
		return "keep"
	case Unwrap:
		return "unwrap"
	}
	return fmt.Sprintf("Action(%d)", uint8(a))
}

// Rule is one normalized rule. Field semantics are documented on
// linkclean.Rule (design §3, §4).
type Rule struct {
	ID     string
	Action Action

	Affiliate    bool
	Network      string
	IDParam      string
	IDPathRegexp string

	Tags []string
	Ref  string

	Include    []string
	Exclude    []string
	URLRegexps []string

	Params       []string
	ParamRegexps []string
	InFragment   bool

	PathRegexps []string

	Unwrap *UnwrapSpec
}

// UnwrapSpec mirrors linkclean.UnwrapSpec.
type UnwrapSpec struct {
	QueryParams   []string
	AllowSameSite bool
	PrependScheme string
}

// Set mirrors linkclean.RuleSet.
type Set struct {
	Name     string
	License  string
	Upstream string
	Commit   string
	SHA256   string
	Notice   string
	Rules    []Rule
}

// Error reports one invalid rule, upstream entry or config entry. The root
// package converts it to *linkclean.RuleError field for field.
type Error struct {
	Set    string // rule-set name, e.g. "brave/clean-urls"; "" for Config fields
	RuleID string // rule ID within Set, or "#<index>" for an upstream element without one
	Field  string // e.g. "include[2]", "unwrap.queryParams", "ExtraArchivePatterns[0]"
	Err    error
}

func (e *Error) Error() string {
	where := e.Set
	if e.RuleID != "" {
		where += ":" + e.RuleID
	}
	if e.Field != "" {
		if where != "" {
			where += ": "
		}
		where += e.Field
	}
	if where == "" {
		return fmt.Sprintf("linkclean: %v", e.Err)
	}
	return fmt.Sprintf("linkclean: %s: %v", where, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }
