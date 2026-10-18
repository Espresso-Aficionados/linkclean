package discordlink

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

// Processor cleans one URL. Offline adapts a Cleaner; a network unshortener
// (planned as a separate package) can implement it too.
type Processor interface {
	Process(ctx context.Context, rawURL string) (linkclean.Result, error)
}

// Offline adapts a Cleaner (Process never returns an error). Its Process
// returns c.Explain(rawURL) and never blocks, so Process calls it on the
// caller's goroutine. Offline panics at once if c is nil.
func Offline(c *linkclean.Cleaner) Processor {
	if c == nil {
		panic("discordlink: Offline of a nil Cleaner")
	}
	return offline{c}
}

// offline is Offline's Processor. Process recognizes it and calls it inline.
type offline struct{ c *linkclean.Cleaner }

func (o offline) Process(_ context.Context, rawURL string) (linkclean.Result, error) {
	return o.c.Explain(rawURL), nil
}

// Change is one link whose cleaned URL differs, as Process returns it.
type Change struct {
	Link   Link
	Result linkclean.Result // Result.Changed is always true
	Err    error            // informational (e.g. unshorten failure); Result is still usable
}

// Options bounds the work Process does for one message.
type Options struct {
	Concurrency int // parallel Process calls; 0 (or negative) means 4
	MaxLinks    int // distinct links (by target before cleaning, in Find order) examined per message; 0 (or negative) means 10 (bounds work per message)
}

// Process runs p over the first MaxLinks distinct links in content and returns
// only the changed ones, de-duplicated by Result.URL, in first-occurrence order.
// unexamined counts the distinct links that were not processed: those beyond
// MaxLinks and those not started before ctx was done (pass it to
// RepostOptions.Unexamined so a reply can say so).
// ctx bounds the whole message. Process scans all of content with Find,
// whose memory grows with its length (see Find).
//
// p receives each link's target as Discord links it: Link.URL, except that
// in a masked link's target a backslash before anything but an ASCII letter,
// an ASCII digit or whitespace is dropped, as Discord's markdown does, so
// [l](https://a.com/x\_y) gives p https://a.com/x_y. Change.Link.URL stays
// the raw span. Links are distinct by that target, before cleaning; the
// first occurrence in Find order is the one processed. A result is kept iff its Changed is true: an error with an
// unchanged result is dropped, and an error with a changed result is kept in
// Change.Err. When two links clean to the same Result.URL, the first one's
// Change, including its Link and Link.Spoiler, is kept. cs is nil when
// nothing changed.
//
// ctx.Err() is checked before each call starts, on every path. Calls run on
// the caller's goroutine when Concurrency is 1, p comes from Offline or only
// one link is examined; otherwise at most Concurrency calls run at once, each
// on its own goroutine, and Process returns only after every started call has
// returned. ctx is passed to p unchanged: Process adds no timeout of its own.
// A panic in p.Process is re-raised on the caller's goroutine once the other
// calls have returned.
func Process(ctx context.Context, content string, p Processor, o Options) (cs []Change, unexamined int) {
	jobs := distinct(Find(content))
	limit := o.MaxLinks
	if limit <= 0 {
		limit = defaultMaxLinks
	}
	if len(jobs) > limit {
		unexamined = len(jobs) - limit
		jobs = jobs[:limit]
	}
	if len(jobs) == 0 {
		return nil, unexamined
	}
	workers := o.Concurrency
	if workers <= 0 {
		workers = defaultConcurrency
	}
	workers = min(workers, len(jobs))

	out := make([]outcome, len(jobs))
	if runsInline(p, workers) {
		runInline(ctx, p, jobs, out)
	} else {
		runPool(ctx, p, jobs, out, workers)
	}

	var seen map[string]bool // the Result.URLs in cs
	for i, oc := range out {
		if !oc.started {
			unexamined++
			continue
		}
		if !oc.r.Changed || seen[oc.r.URL] {
			continue
		}
		if seen == nil {
			seen = make(map[string]bool)
		}
		seen[oc.r.URL] = true
		cs = append(cs, Change{Link: jobs[i], Result: oc.r, Err: oc.err})
	}
	return cs, unexamined
}

const (
	defaultConcurrency = 4
	defaultMaxLinks    = 10
)

// outcome is one job's result. started is false for a job that never ran
// because ctx was done (or, in the pool, because another call panicked).
type outcome struct {
	r       linkclean.Result
	err     error
	started bool
}

// distinct keeps the first Link per target, in Find order. It filters links
// in place.
func distinct(links []Link) []Link {
	if len(links) < 2 {
		return links
	}
	seen := make(map[string]bool, len(links))
	out := links[:0]
	for _, l := range links {
		if u := target(l); !seen[u] {
			seen[u] = true
			out = append(out, l)
		}
	}
	return out
}

// target is the URL Discord links for l. simple-markdown unescapes a masked
// link's target with /\\([^0-9A-Za-z\s])/g; bare links and autolinks are
// taken as written.
func target(l Link) string {
	if l.Kind != Masked && l.Kind != MaskedSuppressed || !strings.Contains(l.URL, `\`) {
		return l.URL
	}
	u := l.URL
	b := make([]byte, 0, len(u))
	for i := 0; i < len(u); i++ {
		if u[i] == '\\' && i+1 < len(u) {
			r, n := utf8.DecodeRuneInString(u[i+1:])
			if (r >= utf8.RuneSelf || !isAlnum(u[i+1])) && !isJSSpace(r) {
				b = append(b, u[i+1:i+1+n]...)
				i += n
				continue
			}
		}
		b = append(b, u[i])
	}
	return string(b)
}

// runsInline reports whether Process calls p on the caller's goroutine.
func runsInline(p Processor, workers int) bool {
	_, ok := p.(offline)
	return ok || workers <= 1
}

// runInline calls p for each job in order on the caller's goroutine,
// stopping before the first job that would start after ctx is done. A panic
// in p propagates unchanged.
func runInline(ctx context.Context, p Processor, jobs []Link, out []outcome) {
	for i, l := range jobs {
		if ctx.Err() != nil {
			return
		}
		out[i].started = true
		out[i].r, out[i].err = p.Process(ctx, target(l))
	}
}

// runPool calls p for the jobs on workers goroutines, which claim job
// indexes in order and store each result at its index. A worker stops when
// the jobs run out, ctx is done or a call has panicked. runPool returns once
// every worker has stopped, then re-raises the first panic, if any.
func runPool(ctx context.Context, p Processor, jobs []Link, out []outcome, workers int) {
	var (
		next     atomic.Int64
		panicked atomic.Bool
		pv       any // the first panic value; written once, by the CAS winner
		wg       sync.WaitGroup
	)
	call := func(i int) {
		defer func() {
			// recover is nil only for panic(nil) under GODEBUG=panicnil=1;
			// that call then counts as returning a zero Result.
			if v := recover(); v != nil && panicked.CompareAndSwap(false, true) {
				pv = v
			}
		}()
		out[i].started = true
		out[i].r, out[i].err = p.Process(ctx, target(jobs[i]))
	}
	for range workers {
		wg.Go(func() {
			for !panicked.Load() {
				i := int(next.Add(1) - 1)
				if i >= len(jobs) || ctx.Err() != nil {
					return
				}
				call(i)
			}
		})
	}
	wg.Wait()
	if panicked.Load() {
		panic(pv)
	}
}
