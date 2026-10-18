// Package linkclean removes tracking parameters from http(s) URLs and unwraps
// known redirect wrappers. It is a pure, offline string transform: no function
// in this package performs I/O. Network unshortening is the separate, opt-in
// package unshorten; this one never will.
//
// Rules come from Brave's clean-urls.json, query-filter.json and debounce.json
// (MPL-2.0, embedded unmodified from one pinned brave/adblock-lists commit) plus
// an MIT overlay maintained in this repository, and a classification table
// (MPL-2.0) that classes every debounce pattern as affiliate, unwrap or off.
// Rules never change at runtime; a data refresh is a patch release.
//
// Cleaning is a minimal edit. Only removed query/fragment segments, removed path
// text, a re-encoded nested URL value, or a replaced wrapper change; once one of
// those happened, empty query segments ("&&", a bare "?") are dropped too. Host
// spelling, percent-encoding, parameter order, port, userinfo and fragment are
// otherwise copied verbatim. If nothing applies, the input string is returned unchanged, so
//
//	if out := linkclean.Clean(in); out != in { /* changed */ }
//
// is a supported change test.
//
// Input is returned unchanged when it is not http(s), unparseable, ambiguous
// (any byte <= 0x20 or 0x7F anywhere, '\' in the authority, '%' in the host,
// bad brackets), over-long, or an archive or security-rewriter URL, and when
// the rules reach no fixed point within 8 strip rounds, which is possible only
// with Config.Extra or custom Sources (see Result.Skipped).
//
// Guarantees (fuzz-tested):
//   - Clean(Clean(s)) == Clean(s), for any rule set: a call that reaches the
//     round cap returns its input unchanged (SkipRoundCap).
//   - If Clean(s) != s, the output contains no byte <= 0x20 and no 0x7F.
//   - Explain(s).URL == Clean(s).
//
// Host matching is always done on the parsed host (userinfo removed, IDNA and
// trailing-dot canonicalized), never by substring search over the URL.
//
// # Licensing
//
// The code is MIT. The embedded Brave lists (rules/brave) and the debounce
// classification table (rules/overlay/debounce-classes.json) are MPL-2.0. A
// program that distributes a binary built with this package must tell
// recipients where the MPL-covered source is (MPL-2.0 §3.2): this module's
// source with rules/brave/NOTICE and NOTICE.md suffices, and
// Cleaner.Notices returns the same attribution at run time, for an about page
// or a --licenses flag. Importers have no other obligations for their own
// code.
//
// # Deviations from Brave
//
// Each deliberate difference from brave-core is pinned by a TestDeviation_DVn
// test:
//
//   - DV-1: query-filter.json is matched with the same URLPattern subset as
//     clean-urls.json, on the parsed host, not by url_pattern_index substring
//     search over the whole URL.
//   - DV-2: parameter names are compared canonically (%XX decoded once, ASCII
//     lowercased), not exactly.
//   - DV-3: "k", "k=" and "k=v" are all stripped, not only non-empty values.
//   - DV-4: one decision per segment, iterated to a fixed point; "=k=v" has an
//     empty name and is never stripped.
//   - DV-5: an exclude pattern that fails to parse fails the build instead of
//     being ignored.
//   - DV-6: only the host is canonicalized for matching; no GURL path
//     canonicalization.
//   - DV-7: unchanged input is returned byte-identical, a bare "?" included.
//   - DV-8: brave-core's conditional query-string trackers are overlay Keep
//     rules with URLRegexps.
//   - DV-9: urldefense hosts are in the archive guard.
//   - DV-10: Keep URLRegexps see the current round's URL, not the original.
//   - DV-11: a host containing '%' is ambiguous and the input is returned
//     unchanged.
//   - DV-12: debounce destinations are decoded with '+' kept literal.
//   - DV-13: debounce prepend_scheme accepts a value that already has a scheme.
//   - DV-14: the debounce param is matched canonically.
package linkclean
