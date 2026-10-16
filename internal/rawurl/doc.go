// Package rawurl splits a raw http(s) URL into byte spans without
// normalizing it, and derives the canonical views used for matching (design
// §4.1): the host view (lowercase, trailing dots trimmed, IDNA, IP flags), the
// ICANN-only site, the match string and pathForRequest. It also applies the
// ambiguity skips, validates unwrap targets independently of their wrapper
// (§6 VALID) and re-serializes them (§5.4). Views exist only for matching;
// output is never built from them.
package rawurl
