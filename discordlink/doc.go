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
// Repost guarantees the shape of its bodies, not what Discord does with
// them:
//
//   - MaxLen counts bytes. Discord counts characters, so a body can end
//     earlier than Discord would require, never later.
//   - Caller text (Header, Annotate's lines, the Overflow line) loses only
//     its CR and LF bytes. Repost does not escape markdown, balance spoiler
//     bars or filter mentions in it, and keeps U+0085, U+2028 and U+2029,
//     which some clients may render as line breaks. Unbalanced '*', '_' or
//     '~' in caller lines can pair with the raw ones URL lines may carry.
//   - URL lines are encoded, not validated. Repost does not check that a
//     URL is http(s) or well formed; under WrapNone a URL that Discord would
//     not link bare in full (including one without a lowercase http:// or
//     https:// scheme) is sent as <…>, which Discord links only for a
//     scheme it accepts. The bare-link check uses the unverified ')' rule
//     (see Unverified grammar), so a URL ending in ')' may be wrapped when
//     Discord would have linked it bare, or the reverse.
//   - The encoded URL is the same URL by RFC 3986 and RFC 3987, but a
//     server that treats a raw byte and its %XX form differently sees a
//     different request. Other bytes RFC 3986 does not allow raw are kept:
//     '\' because browsers read it as '/', and '"', '^', '{' and '}'
//     because they have no effect on Discord's grammar inside a URL line.
//   - Bodies are only safe to send with allowed_mentions = none: a URL may
//     contain '@' and mention-like text, and caller lines are unfiltered.
//   - Room for the overflow line is reserved whenever it could be needed,
//     so a body can be up to one overflow line shorter than MaxLen even
//     when no overflow line follows.
//   - When no block fits, Repost returns nil and the omitted count is lost
//     with it: a link that cannot be shown with its required lines yields
//     no reply at all. A URL whose line alone exceeds MaxLen is omitted,
//     never truncated.
//   - Repost does not de-duplicate; Process does, by Result.URL.
package discordlink
