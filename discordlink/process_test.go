package discordlink

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
)

// The inputs here are bare links separated by spaces and ||spoilers||, the
// subset of the grammar every version of Find agrees on.

// fakeProc is a Processor that records its calls. fn, if set, computes the
// result; otherwise every URL changes to URL+"/c".
type fakeProc struct {
	fn func(ctx context.Context, u string) (linkclean.Result, error)

	mu    sync.Mutex
	calls []string
	gids  []uint64

	inflight, maxInflight, finished atomic.Int32
}

func (f *fakeProc) Process(ctx context.Context, u string) (linkclean.Result, error) {
	n := f.inflight.Add(1)
	for m := f.maxInflight.Load(); n > m && !f.maxInflight.CompareAndSwap(m, n); m = f.maxInflight.Load() {
	}
	defer func() {
		f.inflight.Add(-1)
		f.finished.Add(1)
	}()
	f.mu.Lock()
	f.calls = append(f.calls, u)
	f.gids = append(f.gids, goid())
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, u)
	}
	return result(u, u+"/c"), nil
}

func (f *fakeProc) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// result is a Result for in cleaned to out.
func result(in, out string) linkclean.Result {
	return linkclean.Result{Input: in, URL: out, Changed: in != out}
}

// goid returns the current goroutine's ID.
func goid() uint64 {
	var b [64]byte
	s := strings.TrimPrefix(string(b[:runtime.Stack(b[:], false)]), "goroutine ")
	s, _, _ = strings.Cut(s, " ")
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		panic("goid: " + err.Error())
	}
	return id
}

// linkURLs returns n distinct bare link URLs.
func linkURLs(n int) []string {
	us := make([]string, n)
	for i := range us {
		us[i] = "https://a.com/" + strconv.Itoa(i)
	}
	return us
}

// message joins us with spaces and checks that Find sees exactly us.
func message(t *testing.T, us ...string) string {
	t.Helper()
	m := strings.Join(us, " ")
	checkFind(t, m, us)
	return m
}

// checkFind checks that Find(m) reports the URLs want, so that a failure is
// not blamed on Process when Find disagrees with the test's input.
func checkFind(t *testing.T, m string, want []string) {
	t.Helper()
	var got []string
	for _, l := range Find(m) {
		got = append(got, l.URL)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Find(%q) = %q, want %q", m, got, want)
	}
}

func changeURLs(cs []Change) []string {
	var us []string
	for _, c := range cs {
		us = append(us, c.Result.URL)
	}
	return us
}

func suffixed(us []string, sfx string) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u + sfx
	}
	return out
}

func TestProcessNoLinks(t *testing.T) {
	for _, m := range []string{"", "no links here", "a.com/x www.a.com HTTPS://A.COM/x"} {
		f := &fakeProc{}
		cs, un := Process(context.Background(), m, f, Options{})
		if cs != nil || un != 0 || len(f.called()) != 0 {
			t.Errorf("%q: cs %v, unexamined %d, calls %q; want nothing", m, cs, un, f.called())
		}
	}
}

// TestProcessOrder: results come back in Find order whatever order the calls
// finish in.
func TestProcessOrder(t *testing.T) {
	us := linkURLs(10)
	m := message(t, us...)
	for iter := range 20 {
		rng := rand.New(rand.NewPCG(uint64(iter), 1))
		delays := make(map[string]time.Duration)
		for _, u := range us {
			delays[u] = time.Duration(rng.IntN(2000)) * time.Microsecond
		}
		f := &fakeProc{fn: func(_ context.Context, u string) (linkclean.Result, error) {
			time.Sleep(delays[u])
			return result(u, u+"/c"), nil
		}}
		cs, un := Process(context.Background(), m, f, Options{Concurrency: 4})
		if got, want := changeURLs(cs), suffixed(us, "/c"); !slices.Equal(got, want) || un != 0 {
			t.Fatalf("iter %d: got %q, unexamined %d; want %q, 0", iter, got, un, want)
		}
		for i, c := range cs {
			if c.Link.URL != us[i] || c.Result.Input != us[i] || c.Err != nil {
				t.Errorf("iter %d: change %d = %+v", iter, i, c)
			}
		}
	}
}

// TestProcessMaxLinks: MaxLinks counts distinct raw URLs in Find order;
// repeats are neither processed again nor counted as unexamined.
func TestProcessMaxLinks(t *testing.T) {
	us := linkURLs(12)
	withDups := append(append([]string{}, us[:3]...), us[0], us[1])
	withDups = append(withDups, us[3:]...)
	withDups = append(withDups, us[11], us[5])
	for _, c := range []struct {
		name     string
		in       []string
		max      int
		calls    []string
		unexamin int
	}{
		{"12 distinct, default", us, 0, us[:10], 2},
		{"12 distinct, negative", us, -1, us[:10], 2},
		{"12 distinct with repeats", withDups, 0, us[:10], 2},
		{"MaxLinks 3", withDups, 3, us[:3], 9},
		{"MaxLinks 12", withDups, 12, us, 0},
		{"MaxLinks above count", us[:4], 50, us[:4], 0},
		{"repeats only", []string{us[0], us[0], us[0]}, 1, us[:1], 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := message(t, c.in...)
			for _, conc := range []int{1, 4} {
				f := &fakeProc{}
				cs, un := Process(context.Background(), m, f, Options{Concurrency: conc, MaxLinks: c.max})
				calls := f.called()
				if conc > 1 {
					// The pool's call order is not Find order; its set is.
					calls = sortedLike(calls, c.calls)
				}
				if !slices.Equal(calls, c.calls) || un != c.unexamin {
					t.Errorf("Concurrency %d: calls %q, unexamined %d; want %q, %d", conc, f.called(), un, c.calls, c.unexamin)
				}
				if got, want := changeURLs(cs), suffixed(c.calls, "/c"); !slices.Equal(got, want) {
					t.Errorf("Concurrency %d: changes %q, want %q", conc, got, want)
				}
			}
		})
	}
}

// sortedLike returns got reordered to follow the order of want, if got is a
// permutation of want; otherwise got unchanged.
func sortedLike(got, want []string) []string {
	if len(got) != len(want) {
		return got
	}
	n := make(map[string]int)
	for _, u := range got {
		n[u]++
	}
	for _, u := range want {
		if n[u]--; n[u] < 0 {
			return got
		}
	}
	return want
}

// TestProcessDedup: results are de-duplicated by Result.URL, keeping the
// first occurrence's Change, its Link and Link.Spoiler included (Q10). A raw
// URL repeated in and out of a spoiler is processed once, as its first
// occurrence.
func TestProcessDedup(t *testing.T) {
	x, y, z := "https://a.com/x", "https://a.com/y", "https://a.com/z"
	same := func(_ context.Context, u string) (linkclean.Result, error) {
		if u == z {
			return result(u, z+"/c"), nil
		}
		return result(u, "https://a.com/"), nil
	}
	for _, c := range []struct {
		name    string
		in      string
		links   []string
		want    []Link // the kept Changes' Links, offsets included
		results []string
	}{
		{"spoiler first", "||" + x + "|| " + y + " " + z, []string{x, y, z},
			[]Link{{URL: x, Start: 2, End: 17, Kind: Bare, Spoiler: true}, {URL: z, Start: 36, End: 51, Kind: Bare}},
			[]string{"https://a.com/", z + "/c"}},
		{"plain first", x + " ||" + y + "|| " + z, []string{x, y, z},
			[]Link{{URL: x, Start: 0, End: 15, Kind: Bare}, {URL: z, Start: 36, End: 51, Kind: Bare}},
			[]string{"https://a.com/", z + "/c"}},
		{"raw repeat, spoiler first", "||" + x + "|| " + x, []string{x, x},
			[]Link{{URL: x, Start: 2, End: 17, Kind: Bare, Spoiler: true}},
			[]string{"https://a.com/"}},
		{"raw repeat, plain first", x + " ||" + x + "||", []string{x, x},
			[]Link{{URL: x, Start: 0, End: 15, Kind: Bare}},
			[]string{"https://a.com/"}},
		{"later duplicate result", z + " " + x + " " + y, []string{z, x, y},
			[]Link{{URL: z, Start: 0, End: 15, Kind: Bare}, {URL: x, Start: 16, End: 31, Kind: Bare}},
			[]string{z + "/c", "https://a.com/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			checkFind(t, c.in, c.links)
			for _, conc := range []int{1, 4} {
				f := &fakeProc{fn: same}
				cs, un := Process(context.Background(), c.in, f, Options{Concurrency: conc})
				var links []Link
				for _, ch := range cs {
					links = append(links, ch.Link)
				}
				if !reflect.DeepEqual(links, c.want) || !slices.Equal(changeURLs(cs), c.results) || un != 0 {
					t.Errorf("Concurrency %d: links %+v, results %q, unexamined %d; want %+v, %q, 0",
						conc, links, changeURLs(cs), un, c.want, c.results)
				}
				if n := len(f.called()); n != len(distinctStrings(c.links)) {
					t.Errorf("Concurrency %d: %d calls, want one per distinct raw URL", conc, n)
				}
			}
		})
	}
}

func distinctStrings(ss []string) []string {
	var out []string
	for _, s := range ss {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// TestProcessErr: a result is kept iff Changed; an error rides along on a
// kept result and drops an unchanged one. cs is nil when nothing changed.
func TestProcessErr(t *testing.T) {
	us := linkURLs(4)
	m := message(t, us...)
	errA, errB := errors.New("a"), errors.New("b")
	f := func(_ context.Context, u string) (linkclean.Result, error) {
		switch u {
		case us[0]:
			return result(u, u+"/c"), errA // changed, with an error: kept
		case us[1]:
			return result(u, u), errB // unchanged, with an error: dropped
		case us[2]:
			return result(u, u), nil // unchanged: dropped
		}
		// Changed is what counts, not URL != Input.
		return linkclean.Result{Input: u, URL: u + "/c"}, nil
	}
	for _, conc := range []int{1, 4} {
		cs, un := Process(context.Background(), m, &fakeProc{fn: f}, Options{Concurrency: conc})
		if len(cs) != 1 || un != 0 || cs[0].Link.URL != us[0] || cs[0].Result.URL != us[0]+"/c" || cs[0].Err != errA {
			t.Errorf("Concurrency %d: %+v, unexamined %d", conc, cs, un)
		}
	}
	unchanged := func(_ context.Context, u string) (linkclean.Result, error) { return result(u, u), errB }
	for _, conc := range []int{1, 4} {
		cs, un := Process(context.Background(), m, &fakeProc{fn: unchanged}, Options{Concurrency: conc})
		if cs != nil || un != 0 {
			t.Errorf("Concurrency %d, nothing changed: cs %#v, unexamined %d; want nil, 0", conc, cs, un)
		}
	}
}

// TestProcessCtxDone: with ctx done before Process starts, no call starts
// and every distinct link is unexamined, on every path (Q9).
func TestProcessCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	us := linkURLs(12)
	m := message(t, append(us, us[0])...)
	for _, conc := range []int{0, 1, 4} {
		for _, max := range []int{0, 3, 20} {
			f := &fakeProc{}
			cs, un := Process(ctx, m, f, Options{Concurrency: conc, MaxLinks: max})
			if cs != nil || un != 12 || len(f.called()) != 0 {
				t.Errorf("fake, Concurrency %d, MaxLinks %d: cs %v, unexamined %d, calls %q; want nil, 12, none",
					conc, max, cs, un, f.called())
			}
			cs, un = Process(ctx, m, Offline(linkclean.Default()), Options{Concurrency: conc, MaxLinks: max})
			if cs != nil || un != 12 {
				t.Errorf("Offline, Concurrency %d, MaxLinks %d: cs %v, unexamined %d; want nil, 12", conc, max, cs, un)
			}
		}
	}
	// A single link takes the inline path too.
	cs, un := Process(ctx, us[0], Offline(linkclean.Default()), Options{})
	if cs != nil || un != 1 {
		t.Errorf("one link: cs %v, unexamined %d; want nil, 1", cs, un)
	}
}

// TestProcessCtxCancelInline: a cancel during call k lets call k finish and
// starts no later call.
func TestProcessCtxCancelInline(t *testing.T) {
	us := linkURLs(12)
	m := message(t, us...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeProc{fn: func(_ context.Context, u string) (linkclean.Result, error) {
		if u == us[2] {
			cancel()
		}
		return result(u, u+"/c"), nil
	}}
	cs, un := Process(ctx, m, f, Options{Concurrency: 1})
	if !slices.Equal(f.called(), us[:3]) || !slices.Equal(changeURLs(cs), suffixed(us[:3], "/c")) || un != 2+7 {
		t.Errorf("calls %q, changes %q, unexamined %d; want 3 calls and changes, 9", f.called(), changeURLs(cs), un)
	}
}

// TestProcessCtxCancelPool: two calls run at once; the first cancels ctx.
// Both finish before Process returns, and nothing else starts.
func TestProcessCtxCancelPool(t *testing.T) {
	us := linkURLs(10)
	m := message(t, us...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var started atomic.Int32
	both := make(chan struct{})
	var lateDone atomic.Bool
	f := &fakeProc{fn: func(ctx context.Context, u string) (linkclean.Result, error) {
		if started.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			return linkclean.Result{}, errors.New("the second call never started")
		}
		if u == us[0] {
			cancel()
		} else {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			lateDone.Store(true)
		}
		return result(u, u+"/c"), nil
	}}
	cs, un := Process(ctx, m, f, Options{Concurrency: 2})
	if !lateDone.Load() || f.inflight.Load() != 0 {
		t.Error("Process returned before every started call had")
	}
	if got := sortedLike(f.called(), us[:2]); !slices.Equal(got, us[:2]) || un != 8 {
		t.Errorf("calls %q, unexamined %d; want %q, 8", f.called(), un, us[:2])
	}
	if !slices.Equal(changeURLs(cs), suffixed(us[:2], "/c")) {
		t.Errorf("changes %q", changeURLs(cs))
	}
}

// TestProcessConcurrency: at most Concurrency calls run at once, 4 by
// default, each on a goroutine of its own; with 1 they run in order on the
// caller's goroutine.
func TestProcessConcurrency(t *testing.T) {
	us := linkURLs(16)
	m := message(t, us...)
	for _, c := range []struct{ conc, want int }{{0, 4}, {-3, 4}, {1, 1}, {2, 2}, {3, 3}, {4, 4}, {8, 8}, {100, 16}} {
		// The first calls wait until want of them run at once, then every
		// call lingers, so that a pool with more workers would exceed want.
		f := &fakeProc{}
		var once sync.Once
		full := make(chan struct{})
		f.fn = func(_ context.Context, u string) (linkclean.Result, error) {
			if int(f.inflight.Load()) >= c.want {
				once.Do(func() { close(full) })
			}
			select {
			case <-full:
			case <-time.After(5 * time.Second):
			}
			time.Sleep(2 * time.Millisecond)
			return result(u, u+"/c"), nil
		}
		cs, un := Process(context.Background(), m, f, Options{Concurrency: c.conc, MaxLinks: 16})
		if len(cs) != 16 || un != 0 {
			t.Fatalf("Concurrency %d: %d changes, unexamined %d", c.conc, len(cs), un)
		}
		if got := int(f.maxInflight.Load()); got != c.want {
			t.Errorf("Concurrency %d: %d calls at once, want %d", c.conc, got, c.want)
		}
		me := goid()
		inline := 0
		for _, g := range f.gids {
			if g == me {
				inline++
			}
		}
		if c.want == 1 {
			if inline != 16 || !slices.Equal(f.called(), us) {
				t.Errorf("Concurrency 1: %d of 16 calls on the caller's goroutine, order %q", inline, f.called())
			}
		} else if inline != 0 {
			t.Errorf("Concurrency %d: %d calls on the caller's goroutine", c.conc, inline)
		}
	}
	// A single link is called inline whatever Concurrency is.
	f := &fakeProc{}
	Process(context.Background(), message(t, us[0]), f, Options{Concurrency: 4})
	if len(f.gids) != 1 || f.gids[0] != goid() {
		t.Error("a single link was not processed on the caller's goroutine")
	}
}

// TestProcessOfflineInline: Offline's Processor is called on the caller's
// goroutine at any Concurrency; other Processors only at 1 or one link.
func TestProcessOfflineInline(t *testing.T) {
	off := Offline(linkclean.Default())
	for _, w := range []int{1, 2, 4, 100} {
		if !runsInline(off, w) {
			t.Errorf("Offline, %d workers: not inline", w)
		}
		if got := runsInline(&fakeProc{}, w); got != (w == 1) {
			t.Errorf("fake, %d workers: inline = %v", w, got)
		}
	}
}

var errBoom = errors.New("boom")

// TestProcessPanic: a panic in p.Process reaches the caller's goroutine with
// its value, after every other started call has returned (Q11).
func TestProcessPanic(t *testing.T) {
	us := linkURLs(8)
	m := message(t, us...)
	for _, conc := range []int{1, 3} {
		t.Run(fmt.Sprint("Concurrency ", conc), func(t *testing.T) {
			var slowDone atomic.Bool
			f := &fakeProc{fn: func(_ context.Context, u string) (linkclean.Result, error) {
				switch u {
				case us[0]:
					if conc > 1 {
						time.Sleep(20 * time.Millisecond) // still running when us[1] panics
						slowDone.Store(true)
					}
				case us[1]:
					panic(errBoom)
				}
				return result(u, u+"/c"), nil
			}}
			v, returned := recoverProcess(func() { Process(context.Background(), m, f, Options{Concurrency: conc}) })
			if returned || v != errBoom {
				t.Fatalf("returned %v, recovered %v; want a panic with errBoom", returned, v)
			}
			if f.inflight.Load() != 0 {
				t.Error("Process panicked while calls were still running")
			}
			if conc == 1 && !slices.Equal(f.called(), us[:2]) {
				t.Errorf("inline calls %q, want the first two", f.called())
			}
			if conc > 1 && !slowDone.Load() {
				t.Error("the slow call had not returned")
			}
		})
	}
	// panic(nil) arrives as a *runtime.PanicNilError, not as a silent return.
	f := &fakeProc{fn: func(context.Context, string) (linkclean.Result, error) { panic(nil) }}
	v, returned := recoverProcess(func() { Process(context.Background(), m, f, Options{}) })
	var pne *runtime.PanicNilError
	if err, _ := v.(error); returned || !errors.As(err, &pne) {
		t.Errorf("panic(nil): returned %v, recovered %v", returned, v)
	}
}

// recoverProcess runs fn and reports what it panicked with, or returned =
// true if it returned normally.
func recoverProcess(fn func()) (v any, returned bool) {
	defer func() {
		if !returned {
			v = recover()
		}
	}()
	fn()
	return nil, true
}

func TestOffline(t *testing.T) {
	c := linkclean.Default()
	off := Offline(c)
	for _, u := range []string{"https://example.com/?utm_source=x&id=1", "https://example.com/plain", "not a url"} {
		r, err := off.Process(context.Background(), u)
		if err != nil || !reflect.DeepEqual(r, c.Explain(u)) {
			t.Errorf("%q: %+v, %v; want Explain's result and no error", u, r, err)
		}
	}
	// ctx is ignored: Explain never blocks.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := off.Process(ctx, "https://example.com/?utm_source=x"); err != nil || !r.Changed {
		t.Errorf("done ctx: %+v, %v", r, err)
	}
}

func TestOfflineNil(t *testing.T) {
	v, returned := recoverProcess(func() { Offline(nil) })
	if returned || v == nil {
		t.Error("Offline(nil) did not panic at once")
	}
}

// TestProcessOffline runs Process end to end through Offline.
func TestProcessOffline(t *testing.T) {
	c := linkclean.Default()
	dirty, clean := "https://example.com/a?utm_source=x", "https://example.com/b"
	dirty2 := "https://example.com/a?utm_medium=y"
	in := "see ||" + dirty + "|| and " + clean + " and " + dirty2 + " and " + dirty
	checkFind(t, in, []string{dirty, clean, dirty2, dirty})
	want := c.Explain(dirty)
	if !want.Changed || c.Explain(clean).Changed || c.Explain(dirty2).URL != want.URL {
		t.Fatalf("test URLs no longer clean as expected: %+v", want)
	}
	for _, conc := range []int{0, 1, 4} {
		cs, un := Process(context.Background(), in, Offline(c), Options{Concurrency: conc})
		if len(cs) != 1 || un != 0 {
			t.Fatalf("Concurrency %d: %+v, unexamined %d", conc, cs, un)
		}
		ch := cs[0]
		if ch.Err != nil || !reflect.DeepEqual(ch.Result, want) || ch.Link != (Link{URL: dirty, Start: 6, End: 6 + len(dirty), Kind: Bare, Spoiler: true}) {
			t.Errorf("Concurrency %d: %+v", conc, ch)
		}
	}
}
