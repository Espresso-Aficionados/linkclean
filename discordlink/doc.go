// Package discordlink finds links in Discord message content using Discord's
// client link grammar, not general-purpose URL heuristics. Each span is exactly
// what Discord makes clickable, within the limits listed under Not modelled and
// Nesting limit, so replacing it never orphans trailing characters. It has no
// dependency on any Discord library.
//
// TODO(m3): grammar summary; Find, Process and Repost in one paragraph each.
//
// Guarantees (fuzz-tested):
//   - TODO(m3)
//
// # Nesting limit
//
// TODO(m3): the depth cap, the Spoiler rule at the cap, trailing delimiter
// bytes in capped spans, and every cap row whose answer differs from the
// uncapped grammar.
//
// # Unverified grammar
//
// TODO(m3): one entry per remaining unverified marker (U1–U4, L1–L3, A1, B1).
//
// # Not modelled
//
// TODO(m3): block rules (subtext, headings, quotes, lists), the look-alike
// label filter, emoji labels.
//
// # Limitations (Repost)
//
// TODO(m3): what Repost cannot guarantee.
package discordlink
