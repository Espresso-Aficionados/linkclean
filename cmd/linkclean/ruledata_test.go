package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Espresso-Aficionados/linkclean"
)

// TestRuleDataError: an error from linkclean.New is reported one line per
// joined error, each with a single "linkclean: " prefix, and with the rule
// data's text escaped: -check runs on unreviewed upstream rule data, whose
// rule IDs are upstream pattern text.
func TestRuleDataError(t *testing.T) {
	_, real := linkclean.New(linkclean.Config{MaxUnwrap: -1, MaxURLLength: -1})
	if real == nil {
		t.Fatal("premise: New accepted a negative MaxUnwrap")
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"New", real, "linkclean: MaxUnwrap: negative: -1\nlinkclean: MaxURLLength: negative: -1\n"},
		{"hostile rule ID", errors.Join(
			&linkclean.RuleError{Set: "brave/clean-urls", RuleID: "*://a\x1b]0;x\a\xe2\x80\xae\xff.example/*", Field: "include[0]", Err: errors.New("bad")},
			&linkclean.RuleError{Set: "brave/debounce", RuleID: "#3", Err: errors.New("c1 \xc2\x9b del \x7f")},
		), `linkclean: brave/clean-urls:*://a\x1b]0;x\a\u202e\xff.example/*: include[0]: bad` + "\n" +
			`linkclean: brave/debounce:#3: c1 \u009b del \x7f` + "\n"},
	}
	for _, tt := range tests {
		var stderr bytes.Buffer
		if code := ruleDataError(env{Stderr: &stderr}, tt.err); code != exitError {
			t.Errorf("%s: code %d, want %d", tt.name, code, exitError)
		}
		if got := stderr.String(); got != tt.want {
			t.Errorf("%s: stderr = %q, want %q", tt.name, got, tt.want)
		}
		ctAssertTerminalSafe(t, tt.name, stderr.String())
	}
}
