package data

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SummaryEnv names the environment variable that makes the root module's
// TestSummaryData write a SummaryData JSON file to the path it holds. That
// test computes what needs the engine (cleaning the corpus at base and at
// head, compiled rule IDs, overlay hygiene); rulegen summary runs it through
// go test, because tools is a stdlib-only module. The base data directory
// comes from LINKCLEAN_BASE_DIR, the head data from the working tree.
const SummaryEnv = "LINKCLEAN_SUMMARY_DATA"

// SummaryData is the JSON contract between the root TestSummaryData (the
// writer) and rulegen summary (the reader). Lists are sorted where the field
// says so, so the output is deterministic.
type SummaryData struct {
	// Errors compiling the base and head data; an engine that failed has
	// empty rule IDs and outputs.
	BaseErrors []string `json:"base_errors"`
	HeadErrors []string `json:"head_errors"`

	// Corpus is every eval-corpus item, in corpus order.
	Corpus []CorpusRow `json:"corpus"`

	// CorpusScopes maps every include pattern of the base or head Brave
	// strip data to whether any corpus input matches it (the ★ flag).
	CorpusScopes map[string]bool `json:"corpus_scopes"`

	// Compiled rule IDs of every built-in set, sorted.
	BaseRuleIDs []string `json:"base_rule_ids"`
	HeadRuleIDs []string `json:"head_rule_ids"`

	// Compiled brave/debounce rule ID → its include patterns, at base and
	// at head.
	BaseDebounceGroups map[string][]string `json:"base_debounce_groups"`
	HeadDebounceGroups map[string][]string `json:"head_debounce_groups"`

	// Overlay hygiene notes for the head data, sorted by kind then text.
	Hygiene []HygieneNote `json:"hygiene"`

	// AffiliateHosts are the hosts of the head's affiliate-classed debounce
	// patterns (a "known network domain" for the stub guess), sorted.
	AffiliateHosts []string `json:"affiliate_hosts"`
}

// CorpusRow is one corpus item cleaned (AffiliateKeep) at base and at head.
type CorpusRow struct {
	ID         string   `json:"id"`
	Input      string   `json:"input"`
	Base       string   `json:"base"`
	Head       string   `json:"head"`
	Expected   string   `json:"expected"`
	Alt        []string `json:"alt_expected,omitempty"`
	BrokenBase bool     `json:"broken_base"`
	BrokenHead bool     `json:"broken_head"`
}

// Hygiene note kinds.
const (
	HygieneStaleKeep  = "stale_keep" // a keep-override whose upstream strip disappeared
	HygieneDuplicated = "duplicated" // an overlay strip now duplicated upstream
	HygieneOverlap    = "overlap"    // overlapping overlay rules
)

// HygieneNote is one overlay hygiene finding.
type HygieneNote struct {
	Kind   string `json:"kind"`
	RuleID string `json:"rule_id"`
	Param  string `json:"param,omitempty"`
	Scope  string `json:"scope,omitempty"`
	Text   string `json:"text"`
}

// ParseSummaryData decodes a SummaryData file strictly, so a writer and
// reader that drift apart fail loudly.
func ParseSummaryData(b []byte) (SummaryData, error) {
	var d SummaryData
	if err := decodeStrict(b, &d); err != nil {
		return SummaryData{}, fmt.Errorf("%s: %w", SummaryEnv, err)
	}
	return d, nil
}

// Encode renders d as indented JSON with a trailing newline.
func (d *SummaryData) Encode() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(d)
	return b.Bytes()
}
