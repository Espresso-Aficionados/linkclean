package linkclean

import (
	"errors"
	"fmt"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// RuleError reports one invalid rule or config entry.
type RuleError struct {
	Set, RuleID, Field string // e.g. "brave/clean-urls", "*://*.amazon.ae/*", "exclude[2]"
	Err                error
}

func (e *RuleError) Error() string {
	return (&rule.Error{Set: e.Set, RuleID: e.RuleID, Field: e.Field, Err: e.Err}).Error()
}

func (e *RuleError) Unwrap() error { return e.Err }

// Warning is a non-fatal configuration problem.
type Warning struct {
	Field string // "DisableRules", "DisableRulePrefixes" or "DisableTags"
	Entry string // the configured entry
	Err   error  // ErrUnknownRule or ErrUnknownTag
}

func (w Warning) Error() string {
	return fmt.Sprintf("%s %q: %v", w.Field, w.Entry, w.Err)
}

var (
	ErrUnknownRule = errors.New("linkclean: DisableRules entry matches no rule")
	ErrUnknownTag  = errors.New("linkclean: DisableTags entry matches no rule")
)
