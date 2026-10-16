// Package query tokenizes raw query strings on '&' (';' is not a separator),
// rebuilds them after removals, and implements canon(name) and the single
// percent-decoding and component encoding used for nested values (design §4.1,
// §5.1, §5.4). It also decides whether a fragment is parameter-shaped (§5.3).
package query
