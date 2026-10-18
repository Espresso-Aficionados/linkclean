package discordlink

// Kind is how a link is written in the message, which decides whether
// Discord previews it. A masked link is MaskedSuppressed only when both the
// '<' and the '>' around its target were consumed: [label](<https://x) is
// Masked, so WouldEmbed errs towards reporting a preview.
type Kind uint8

const (
	Bare             Kind = iota + 1 // https://x               previewed
	Suppressed                       // <https://x>             not previewed
	Masked                           // [label](https://x)      preview unverified (harness pending)
	MaskedSuppressed                 // [label](<https://x>)
)

// Link is one link Find reports.
type Link struct {
	URL        string // == content[Start:End]
	Start, End int    // byte offsets of the URL only (not brackets/label)
	Kind       Kind
	Spoiler    bool   // inside ||…||, or in a span at the nesting limit that contains || (see Nesting limit)
	Label      string // Masked kinds only
}

// WouldEmbed reports whether Discord would generate a preview for l: true for
// Bare and Masked, false for Suppressed and MaskedSuppressed. Spoiler does not
// matter, since Discord previews spoilered links too (behind a blur). That
// masked links are previewed is unverified (see Unverified grammar); true is
// the conservative answer for a caller deciding whether to suppress previews.
func (l Link) WouldEmbed() bool {
	return l.Kind == Bare || l.Kind == Masked
}
