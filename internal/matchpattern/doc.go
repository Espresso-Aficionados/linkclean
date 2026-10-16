// Package matchpattern implements the subset of Chromium's
// extensions::URLPattern that Brave's lists use (scheme "*"/http/https; host
// "*", "*.d" or "d"; optional port; path glob with '*' as the only wildcard,
// matched against pathForRequest in raw and UnescapeRule::NORMAL forms), plus
// linkclean's "label.*" host extension (design §4.2, §4.5.1). Anything outside
// the subset is a parse error. No regexp is ever built from pattern text.
package matchpattern
