package discordlink

// appendURLLine appends the URL line for u to b: u percent-encoded as Repost
// documents, wrapped in <…> under WrapAngle (or under WrapNone when the
// encoded URL would not render as one bare link), then in ||…|| if spoiler.
// The appended length is exactly the length Repost budgets.
func appendURLLine(b []byte, u string, w Wrap, spoiler bool) []byte {
	panic("discordlink: not implemented")
}

// stripCRLF returns s without its CR and LF bytes, allocating only if it has
// any.
func stripCRLF(s string) string {
	panic("discordlink: not implemented")
}
