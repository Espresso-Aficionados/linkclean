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

// SummarySchema is the SummaryData schema this package reads. The writer
// sets it; it changes with any incompatible change.
const SummarySchema = 1

// SummaryData is the JSON contract between the root TestSummaryData (the
// writer, whose summaryData struct this mirrors field for field) and
// rulegen summary (the reader). Lists are sorted where the field says so,
// so the output is deterministic.
type SummaryData struct {
	Schema int `json:"schema"` // SummarySchema

	// Errors compiling the base and head data: "" when that side compiles,
	// "no base" without LINKCLEAN_BASE_DIR. A side that failed has empty
	// rule IDs and outputs.
	BaseError string `json:"base_error"`
	HeadError string `json:"head_error"`

	// Corpus is every eval-corpus item, in corpus order.
	Corpus []CorpusRow `json:"corpus"`

	// CorpusScopes maps every include pattern of the base or head data
	// (Brave files and overlay), and "global", to whether any corpus input
	// matches it (the ★ flag).
	CorpusScopes map[string]bool `json:"corpus_scopes"`

	// Compiled rule IDs, "<set>:<id>", of every built-in set, sorted.
	BaseRuleIDs []string `json:"base_rule_ids"`
	HeadRuleIDs []string `json:"head_rule_ids"`

	// Compiled brave/debounce rule ID ("<set>:<id>") → its include
	// patterns, at base and at head.
	BaseDebounceGroups map[string][]string `json:"base_debounce_groups"`
	HeadDebounceGroups map[string][]string `json:"head_debounce_groups"`

	// Overlay hygiene notes for the head data.
	Hygiene []HygieneNote `json:"hygiene"`

	// AffiliateHosts maps the host (without a leading "*.") of every
	// affiliate-classed debounce pattern at head to its network: a stub's
	// "known network domain" is such a host or a subdomain of one.
	AffiliateHosts map[string]string `json:"affiliate_hosts"`
}

// CorpusRow is one corpus item cleaned (AffiliateKeep) by the head engine
// over base's and over head's data. Outputs are "" on a side that does not
// compile.
type CorpusRow struct {
	ID         string   `json:"id"`
	Input      string   `json:"input"`
	Expected   string   `json:"expected"`
	Alt        []string `json:"alt_expected"`
	Base       string   `json:"base_output"`
	Head       string   `json:"head_output"`
	BrokenBase bool     `json:"base_broken"`
	BrokenHead bool     `json:"head_broken"`
}

// Hygiene note kinds.
const (
	HygieneStaleKeep  = "stale_keep"       // a Keep overriding upstream whose name no Brave strip rule strips in its scope any more
	HygieneDuplicated = "duplicated_strip" // an overlay Strip name that Brave strips in every scope of the rule
	HygieneOverlap    = "overlap"          // an overlay Unwrap include that a brave/debounce rule also covers
)

// HygieneNote is one overlay hygiene finding.
type HygieneNote struct {
	Kind   string `json:"kind"`
	RuleID string `json:"rule"`            // overlay rule ID
	Param  string `json:"param,omitempty"` // canonical name (stale_keep, duplicated_strip)
	Scope  string `json:"scope,omitempty"` // overlay include (overlap)
	With   string `json:"with,omitempty"`  // the Brave rule, "<set>:<id>" (duplicated_strip, overlap)
	AtBase bool   `json:"at_base"`         // the same note holds at base
}

// ParseSummaryData decodes a SummaryData file strictly, so a writer and
// reader that drift apart fail loudly.
func ParseSummaryData(b []byte) (SummaryData, error) {
	var d SummaryData
	if err := decodeStrict(b, &d); err != nil {
		return SummaryData{}, fmt.Errorf("%s: %w", SummaryEnv, err)
	}
	if d.Schema != SummarySchema {
		return SummaryData{}, fmt.Errorf("%s: schema %d, want %d", SummaryEnv, d.Schema, SummarySchema)
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
