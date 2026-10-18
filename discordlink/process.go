package discordlink

import (
	"context"

	"github.com/Espresso-Aficionados/linkclean"
)

// Processor cleans one URL. *unshorten.Resolver implements it; Offline adapts a Cleaner.
type Processor interface {
	Process(ctx context.Context, rawURL string) (linkclean.Result, error)
}

// Offline adapts a Cleaner (Process never returns an error). Its Process
// returns c.Explain(rawURL) and never blocks, so Process calls it on the
// caller's goroutine. Offline panics at once if c is nil.
func Offline(c *linkclean.Cleaner) Processor {
	panic("discordlink: not implemented")
}

type Change struct {
	Link   Link
	Result linkclean.Result // Result.Changed is always true
	Err    error            // informational (e.g. unshorten failure); Result is still usable
}

type Options struct {
	Concurrency int // parallel Process calls; 0 (or negative) means 4
	MaxLinks    int // distinct links (by raw URL, in Find order) examined per message; 0 (or negative) means 10 (bounds work per message)
}

// Process runs p over the first MaxLinks distinct links in content and returns
// only the changed ones, de-duplicated by Result.URL, in first-occurrence order.
// unexamined counts the distinct links that were not processed: those beyond
// MaxLinks and those not started before ctx was done (pass it to
// RepostOptions.Unexamined so a reply can say so).
// ctx bounds the whole message.
//
// Links are distinct by raw URL; the first occurrence in Find order is the
// one processed. A result is kept iff its Changed is true: an error with an
// unchanged result is dropped, and an error with a changed result is kept in
// Change.Err. When two links clean to the same Result.URL, the first one's
// Change, including its Link and Link.Spoiler, is kept. cs is nil when
// nothing changed.
//
// ctx.Err() is checked before each call starts, on every path. Calls run on
// the caller's goroutine when Concurrency is 1 or p comes from Offline;
// otherwise at most Concurrency calls run at once, each on its own goroutine,
// and Process returns only after every started call has returned. ctx is
// passed to p unchanged: Process adds no timeout of its own. A panic in
// p.Process is re-raised on the caller's goroutine once the other calls have
// returned.
func Process(ctx context.Context, content string, p Processor, o Options) (cs []Change, unexamined int) {
	panic("discordlink: not implemented")
}
