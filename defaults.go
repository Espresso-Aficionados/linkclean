package linkclean

import "embed"

// rulesFS holds exactly the data that ships. TestRules validates this FS, so CI
// checks what is embedded.
//
//go:embed rules/brave/*.json rules/overlay/*.json rules.lock.json
var rulesFS embed.FS

// lockFile is rules.lock.json (schema 4, design §11.2). It is written only by
// `rulegen sync`.
type lockFile struct {
	Schema   int `json:"schema"`
	Upstream struct {
		Repo       string                `json:"repo"`
		Submodule  string                `json:"submodule"`
		Branch     string                `json:"branch"`
		Commit     string                `json:"commit"`
		CommitDate string                `json:"commit_date"`
		Files      map[string]lockedFile `json:"files"` // keyed by upstream path, e.g. "brave-lists/clean-urls.json"
	} `json:"upstream"`
}

type lockedFile struct {
	Vendored    string         `json:"vendored"` // repository path, e.g. "rules/brave/clean-urls.json"
	GitBlobSHA1 string         `json:"git_blob_sha1"`
	SHA256      string         `json:"sha256"`
	Counts      map[string]int `json:"counts,omitempty"`
	SPDX        string         `json:"spdx,omitempty"`
}

// DefaultSources returns fresh deep copies of the built-in rule sets, in
// priority order: "brave/clean-urls", "brave/query-filter", "brave/debounce",
// "linkclean/overlay". "brave/debounce" holds the Unwrap rules compiled from
// debounce.json through the overlay's classification table.
func DefaultSources() []RuleSet {
	panic("TODO(WP-D): DefaultSources")
}
