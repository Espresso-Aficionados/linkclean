package unshorten

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/Espresso-Aficionados/linkclean"
)

// rvRaceCorpus is TestResolveRace's corpus: chains, fallbacks, definitive
// failures, wrappers, an unregistered input and a fragment, with the
// servers that answer them.
var rvRaceCorpus = []string{
	"https://t.co/a?utm_source=x",
	"https://amzn.to/b",
	"https://t.co/c",
	"https://amzn.to/e",
	"https://t.co/f",
	"https://l.facebook.com/l.php?u=https%3A%2F%2Ft.co%2Fa&h=AT0",
	"https://example.org/x?utm_source=z",
	"http://t.co/g#frag",
	"https://www.awin1.com/cread.php?awinmid=1&awinaffid=2&ued=https%3A%2F%2Famzn.to%2Fb",
}

func rvRaceNet(t *testing.T) *testNet {
	n := installNet(t)
	n.HandleTLS("t.co", rvRoutes{
		"/a": rvRedirect(301, "https://example.net/p?utm_source=y&id=1"),
		"/c": rvRedirect(301, "https://amzn.to/d"),
		"/g": rvRedirect(302, "https://example.net/g"),
		// /f: 404 (ErrNoRedirect)
	})
	n.HandleTLS("amzn.to", rvRoutes{
		"/b": rvRedirect(301, "https://www.amazon.com/dp/B0?tag=aff-20&linkCode=ll1&ref_=as_li"),
		"/d": rvRedirect(301, "https://www.amazon.com/dp/B1?ref_=x"),
		"/e": rvRedirect(301, "https://www.amazon.com/"),
	})
	return n
}

// rvOutcome is one call's comparable outcome: the Result without Cached
// (which depends on timing), the Process result, and which sentinels the
// error wraps.
type rvOutcome struct {
	res   Result
	proc  linkclean.Result
	errIs []bool
}

var rvAllSentinels = []error{ErrUnresolved, ErrHopLimit, ErrLoop, ErrFallback, ErrNoRedirect, ErrNoMatch,
	ErrBadTarget, ErrFetchDenied, ErrBudget, ErrNotShortLink, ErrTransient, ErrUnguardedTransport}

func rvCall(r *Resolver, raw string) rvOutcome {
	res, err := r.Resolve(context.Background(), raw)
	proc, perr := r.Process(context.Background(), raw)
	res.Cached = false
	if len(res.Hops) == 0 {
		res.Hops = nil
	}
	for _, p := range []*linkclean.Result{&proc, &res.Clean} {
		if len(p.Hops) == 0 {
			p.Hops = nil
		}
		if len(p.Removed) == 0 {
			p.Removed = nil
		}
		if len(p.Kept) == 0 {
			p.Kept = nil
		}
	}
	o := rvOutcome{res: res, proc: proc}
	for _, s := range rvAllSentinels {
		o.errIs = append(o.errIs, errors.Is(err, s), errors.Is(perr, s))
	}
	return o
}

// TestResolveRace runs 8 goroutines over one Resolver and its strip view on
// the corpus and compares every outcome with a serial run on its own
// Resolver, on every field except Cached, which depends on timing (run
// under -race).
func TestResolveRace(t *testing.T) {
	rvRequireEntry(t, "t.co", "amazon")
	n := rvRaceNet(t)
	strip := rvStrip(t)

	serial := rvResolver(t, n, Config{})
	type key struct {
		view int
		raw  string
	}
	want := map[key]rvOutcome{}
	for vi, r := range []*Resolver{serial, serial.WithCleaner(strip)} {
		for _, raw := range rvRaceCorpus {
			want[key{vi, raw}] = rvCall(r, raw)
		}
	}

	shared := rvResolver(t, n, Config{})
	views := []*Resolver{shared, shared.WithCleaner(strip)}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 3 {
				for i := range rvRaceCorpus {
					raw := rvRaceCorpus[(i+g+round)%len(rvRaceCorpus)]
					vi := (g + i) % 2
					if got := rvCall(views[vi], raw); !reflect.DeepEqual(got, want[key{vi, raw}]) {
						t.Errorf("goroutine %d, view %d, %s:\n  got  %+v\n  want %+v", g, vi, raw, got, want[key{vi, raw}])
					}
				}
			}
		}()
	}
	wg.Wait()
}
