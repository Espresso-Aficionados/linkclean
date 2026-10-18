package discordlink

type Wrap uint8

const (
	WrapAngle Wrap = iota // zero value: <url>; suppresses preview without message flags
	WrapNone              // caller sets SUPPRESS_EMBEDS (1<<2) on the message
)

// RepostOptions: every zero value is the safe choice, and the zero value of
// each MA-4 field reproduces the pre-amendment behaviour.
type RepostOptions struct {
	Wrap      Wrap
	NoSpoiler bool // zero: spoilered links are reposted inside ||…||
	// MaxLen 0 (or negative) means 2000 (bot content limit). It is counted in
	// bytes, which is never less than Discord's character count.
	MaxLen int
	// Header is an optional first line of every body, e.g. "Without
	// tracking:". It is caller text with CR and LF removed, counts against
	// each body's MaxLen, and is never a body on its own: a Header that
	// leaves no room for any block yields no bodies.
	Header string

	// Annotate, if non-nil, returns lines placed directly under c's URL line
	// (MA-4). They are CALLER TEXT: Repost removes CR and LF inside each line
	// and otherwise emits it verbatim. It does not escape markdown, wrap
	// spoilers or filter mentions; the callback owns all of that, and it
	// receives c.Link.Spoiler to do so. The URL line above the annotation is
	// Repost's own and never carries a raw '`', '|', '<', '>', bidi or
	// zero-width code point (see Repost). It may carry raw '*', '_' and '~',
	// which are legal in URIs, so caller lines must not leave those
	// delimiters unbalanced, or they can pair across lines. A link's URL line
	// and its annotation lines form one block that is never split across
	// bodies. required (0 ≤ required ≤ len(lines); clamped) is the number of
	// TRAILING lines that must accompany the URL line. Repost calls
	// Annotate(c, false) once for every block it tries to place; blocks
	// counted as omitted once MaxBodies is reached are not passed to it. If a
	// block does not fit an empty body, Repost degrades it in this order:
	//   1. drop the non-required lines from the front, one at a time;
	//   2. if the URL line plus the required lines still do not fit and
	//      required > 0, call Annotate(c, true) once and use only its trailing
	//      required lines (the callback's compact form of the same facts); a
	//      compact call that returns required == 0 or no lines omits the link;
	//   3. if that still does not fit, omit the link and count it, never
	//      showing it without its required lines.
	// With required == 0 the block can degrade to its URL line alone (the
	// pre-round-3 rule). A block that does not fit a non-empty body stops that
	// body. Annotate must be pure: Repost may call it twice for one Change.
	Annotate func(c Change, compact bool) (lines []string, required int)

	// MaxBodies > 0 caps the number of bodies returned (0 or negative: no
	// cap). Blocks that do not fit are counted as omitted. If Overflow is
	// non-nil and omitted+Unexamined > 0, its line, with CR and LF removed, is
	// appended to the last body; Repost reserves room for it whenever it could
	// be needed (a later block exists, Unexamined > 0, or a link has already
	// been omitted), in every body, since any body can turn out to be the
	// last, so the last body still fits MaxLen. The room reserved is that of
	// the longest line Overflow returns for any count Repost could pass, so
	// its length need not grow with the counts; an empty line reserves nothing
	// and is not appended. With Overflow set and Unexamined > 0, room is
	// reserved even for a single block, so a block that alone fills MaxLen is
	// omitted. If no block is emitted at all, Repost returns no bodies: an
	// overflow line is never a body on its own.
	// Overflow must be pure: Repost may call it once for each omitted count it
	// could need (at most len(cs)+1 times).
	// Unexamined is Process's second result; Repost only passes it through
	// (negative counts as 0).
	MaxBodies  int
	Unexamined int
	Overflow   func(omitted, unexamined int) string
}

// Repost renders one or more message bodies, each ≤ MaxLen, splitting only
// between links (a single URL longer than MaxLen is omitted, never truncated).
// Without Annotate, Header and Overflow, bodies contain only cleaned URLs: no
// labels, no param values, no user text. With them, they also contain exactly
// the caller's lines, with CR and LF removed. In every emitted URL line,
// whatever Wrap, '`' and '|' are percent-encoded (%60, %7C): neither is legal
// in an RFC 3986 URI, so the encoded form is the correct spelling of the same
// URL, and raw they would pair with code spans or spoiler bars in the caller's
// lines or in other links' lines. Bidi formatting and zero-width code points
// (U+061C, U+200B–U+200F, U+202A–U+202E, U+2060–U+2064, U+2066–U+2069,
// U+FEFF) are percent-encoded as their UTF-8 bytes (%E2%80%AE): the
// IRI-to-URI mapping (RFC 3987 §3.1) encodes every non-ASCII code point, so
// this is the same URL, and RFC 3987 §4.1 forbids the bidi formatting
// characters in IRIs outright. Raw, an override on a URL line could reorder
// the caller's lines below. '<' and '>' are also percent-encoded (%3C, %3E),
// whatever Wrap: with WrapAngle so the wrapper cannot be broken, and with
// WrapNone because Discord's bare-link rule ends at '<', which could also
// start a mention token. So are the bytes 0x00–0x20 and 0x7F, U+0085, U+2028,
// U+2029 and every byte of invalid UTF-8: none is legal raw in a URI, and a
// line break would split the block. '%' is never encoded, so an
// already-encoded URL is unchanged and encoding is idempotent.
//
// With WrapNone, a URL that would not render as exactly one bare link (one
// ending in a period, comma, colon, semicolon, quote or ']', one ending in an
// unbalanced ')', or a degenerate one) is wrapped in <…> on its line alone;
// the URL is unchanged and Discord hides the brackets. A URL line is wrapped
// in ||…|| when c.Link.Spoiler is set and NoSpoiler is not.
//
// Callers MUST send bodies with allowed_mentions = none.
func Repost(cs []Change, o RepostOptions) []string {
	bodies, _ := repost(cs, o)
	return bodies
}

// repostStats records what one repost call did, for tests.
type repostStats struct {
	emitted, omitted, overflowDropped int
	annotate, compact                 []int // per-Change Annotate calls with compact false and true
}

// repost is Repost with its statistics.
func repost(cs []Change, o RepostOptions) ([]string, repostStats) {
	panic("discordlink: not implemented")
}
