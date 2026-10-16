// Package brave compiles Brave's vendored lists into normalized rules
// (internal/rule): clean-urls.json and query-filter.json into Strip rules
// (design §4.5.2), and debounce.json, through the human-owned classification
// table, into Unwrap rules (§4.5.3). The compilers are strict: they never skip
// or mutate input silently, and every unknown construct is an error.
package brave
