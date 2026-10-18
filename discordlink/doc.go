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
// Find's answers rest on these points of Discord's grammar, which have not
// yet been confirmed against Discord's clients. The tests pin each one, under
// its ID, until a private-channel run settles it:
//   - U1: whether Discord balances ')'. Find drops a bare link's trailing ')'
//     while the link holds more ')' than '(', so
//     https://en.wikipedia.org/wiki/Foo_(bar) keeps its ')' and
//     (https://a.com/x) does not. If Discord does not balance, such links
//     end elsewhere.
//   - U2: whether masked links are previewed. WouldEmbed reports true for
//     Masked, the conservative answer for a caller deciding whether to
//     suppress previews.
//   - U3: upper-case schemes. Only lowercase http:// and https:// start a
//     link: HTTPS://a.com is text, and <HTTPS://a.com> is consumed as an
//     autolink without reporting a link.
//   - U4: server previews versus client links. Find reports what the client
//     makes clickable: a URL in a code span is not a link, though the server
//     might still preview it, and a spoilered link counts as previewed.
//   - L1: the label filter. A masked link whose label contains http:// or
//     https:// is not a masked link; Find reports the bare links in its text
//     instead, so [https://a.com/x](https://b.com/y) is one bare link,
//     https://a.com/x](https://b.com/y).
//   - L2: [label](<https://a.com/x) without the closing '>' is Masked, not
//     MaskedSuppressed (simple-markdown says MaskedSuppressed), so WouldEmbed
//     errs towards reporting a preview.
//   - L3: a masked link whose target is not http(s), such as
//     [l](javascript:x), is consumed and reports nothing. If Discord shows
//     it as plain text instead, bare links in that text would count.
//   - A1: an autolink target may contain a newline, a tab or an NBSP, as the
//     autolink regex allows: <https://a.com/x\ny> is one Suppressed link.
//   - B1: block rules are not modelled (see Not modelled).
//
// # Not modelled
//
// Find models Discord's inline grammar only. It does not model:
//   - Block rules: subtext (-# ), headings (#, ##, ###), block quotes (> and
//     >>> ) and lists. They matter only when a spoiler, emphasis, code span
//     or autolink crosses into or out of a line that starts one. In
//     "-# ||a\n||https://a.com/x||" Find reports the bare link
//     https://a.com/x|| without Spoiler; if Discord scopes inline spans to
//     the subtext line, the link is https://a.com/x and spoilered.
//   - Discord's filter for look-alike characters in masked-link labels.
//   - Emoji in labels, whose rendering depends on the author.
//   - simple-markdown's input normalisation, which turns each tab into four
//     spaces and each CR LF or lone CR into LF, and drops form feeds, before
//     parsing. Find parses the content as sent, so its offsets index it; a
//     tab inside an autolink target, or a form feed inside a link, can make
//     the answer differ from Discord's.
//   - The shrug emoticon, which some Discord markdown parsers read as one
//     token. Find reads its "\_" as an escape and its second '_' as a
//     possible emphasis opener.
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
