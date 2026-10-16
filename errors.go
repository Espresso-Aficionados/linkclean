package linkclean

import (
	"errors"
	"fmt"
)

// RuleError reports one invalid rule or config entry.
type RuleError struct {
	Set, RuleID, Field string // e.g. "brave/clean-urls", "*://*.amazon.ae/*", "exclude[2]"
	Err                error
}

func (e *RuleError) Error() string {
	switch {
	case e.Set == "" && e.RuleID == "":
		return fmt.Sprintf("linkclean: %s: %v", e.Field, e.Err)
	case e.Field == "":
		return fmt.Sprintf("linkclean: %s:%s: %v", e.Set, e.RuleID, e.Err)
	}
	return fmt.Sprintf("linkclean: %s:%s: %s: %v", e.Set, e.RuleID, e.Field, e.Err)
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
