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
// Spoilers, strong, em, underline and strikethrough nest at most 16 deep:
// inside 16 of them, Find does not try them again. Discord has no such
// limit, but a run of '*' or '_' nests about half its length deep, and
// without a limit the scan would be quadratic in the length of such runs.
// In a span at the limit:
//   - escapes, code, autolinks, mentions, masked links and bare links are
//     still found;
//   - every link has Spoiler set if an enclosing span is a spoiler or the
//     span contains "||", so a link Discord would hide is never reported as
//     visible (one it shows can be reported as spoilered);
//   - a link can take in trailing delimiters that Discord would parse as
//     closers, as in https://a.com/x|| or https://a.com/x**.
//
// The inputs that reach the limit are long delimiter runs, such as 34 or
// more '*' on each side of a link. Those runs pair among themselves, not
// around the link, so Find reports the same links for them as the grammar
// without a limit.
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
