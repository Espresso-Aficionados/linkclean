package unshorten

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// hopKey is the Cache and flight key of a hop: e's name, NUL, the names of
// the strategies eligible under p joined by ',', NUL, and the hop URL in
// request form. Entry names hold no NUL, so the key up to its first NUL is
// always the entry name.
func hopKey(e *compiledEntry, p GenericPolicy, reqForm string) string {
	primary, fallback := e.eligible(p)
	names := ""
	if primary != nil {
		names = primary.Name()
	}
	if fallback != nil {
		names += "," + fallback.Name()
	}
	return e.Name + "\x00" + names + "\x00" + reqForm
}

// maxNext bounds a next URL's length as serialized.
const maxNext = 8 << 10

// validateNext validates a strategy's next URL: at most 8 KiB as
// u.String(), accepted by rawurl.ParseTarget, port "", 80 or 443. next is
// u.String(). A u whose serialization does not parse back to itself is
// rejected too, so the next URL a cache record holds is a fixed point of
// url.Parse and String.
func validateNext(u *url.URL) (next string, ok bool) {
	if u == nil {
		return "", false
	}
	s := u.String()
	if len(s) > maxNext {
		return "", false
	}
	t, ok := rawurl.ParseTarget(s)
	if !ok {
		return "", false
	}
	switch t.Port {
	case "", "80", "443":
	default:
		return "", false
	}
	if back, err := url.Parse(s); err != nil || back.String() != s {
		return "", false
	}
	return s, true
}

// chain is the outcome of one resolution: the first clean, the network hops
// and the destination cleans, or a failure. On a failure url is off.URL and
// there are no hops or fins.
type chain struct {
	off    linkclean.Result
	url    string
	hops   []Hop
	fins   []finClean
	cached bool // every hop lookup was served from the Cache
	err    error
}

// finClean is the offline clean of a destination that left the registry,
// or of the URL a stop was served for. after is the index in chain.hops of
// the network hop it follows.
type finClean struct {
	after int
	res   linkclean.Result
}

// lastClean is Result.Clean: the last destination clean, else the first
// clean.
func (ch *chain) lastClean() linkclean.Result {
	if n := len(ch.fins); n > 0 {
		return ch.fins[n-1].res
	}
	return ch.off
}

// assemble builds Process's linkclean.Result: the first clean's hops, then
// each network hop followed by the hops of the clean it led to; Removed and
// Kept are the first clean's, then each destination clean's in hop order.
// Changed is URL != raw, Skipped the first clean's, Signed the last
// clean's, UnwrapAbandoned the OR over every clean. With no network hop
// (no match, a failure, a stop at the first hop) it is the first clean.
func (ch *chain) assemble(raw string) linkclean.Result {
	off := ch.off
	if len(ch.hops) == 0 {
		return off
	}
	res := linkclean.Result{
		Input:           raw,
		URL:             ch.url,
		Changed:         ch.url != raw,
		Skipped:         off.Skipped,
		Signed:          off.Signed,
		UnwrapAbandoned: off.UnwrapAbandoned,
	}
	res.Hops = append(res.Hops, off.Hops...)
	res.Removed = append(res.Removed, off.Removed...)
	res.Kept = append(res.Kept, off.Kept...)
	fi := 0
	for i, h := range ch.hops {
		from, _ := rawurl.Parse(h.From)
		res.Hops = append(res.Hops, linkclean.Hop{
			From: h.From, To: h.To, Rule: "unshorten:" + h.Entry,
			Category: linkclean.CatUnshortened, FromHost: from.Host,
		})
		for ; fi < len(ch.fins) && ch.fins[fi].after == i; fi++ {
			f := ch.fins[fi].res
			res.Hops = append(res.Hops, f.Hops...)
			res.Removed = append(res.Removed, f.Removed...)
			res.Kept = append(res.Kept, f.Kept...)
			res.Signed = f.Signed
			res.UnwrapAbandoned = res.UnwrapAbandoned || f.UnwrapAbandoned
		}
	}
	return res
}

// lookup parses s and looks it up in st's registry under st's policy.
func (st *state) lookup(s string, intermediate bool) (*compiledEntry, rawurl.URL, bool) {
	u, skip := rawurl.Parse(s)
	if skip != rawurl.OK {
		return nil, rawurl.URL{}, false
	}
	e, ok := st.reg.lookup(&u, intermediate, st.cfg.policy)
	return e, u, ok
}

// requestForm is u's request form, upgraded to https unless allowHTTP. u is
// requestable (lookup matched it), so it has no userinfo and an http port
// of "" or 80, which the upgrade drops.
func requestForm(u *rawurl.URL, allowHTTP bool) string {
	if u.Scheme != "http" || allowHTTP {
		return u.RequestForm()
	}
	var b strings.Builder
	b.Grow(len("https://") + len(u.Host) + len(u.Path) + 1 + len(u.Query))
	b.WriteString("https://")
	b.WriteString(u.Host)
	b.WriteString(u.Path)
	if u.HasQuery {
		b.WriteByte('?')
		b.WriteString(u.Query)
	}
	return b.String()
}

// seenForm is the form of s the loop check compares: its request form, or
// s without its fragment when rawurl.Parse refuses it.
func seenForm(s string) string {
	if u, skip := rawurl.Parse(s); skip == rawurl.OK {
		return u.RequestForm()
	}
	s, _, _ = strings.Cut(s, "#")
	return s
}

// isFallback runs e.IsFallback on the hop URL in request form and its next
// URL. A URL that does not parse is not a fallback.
func isFallback(e *compiledEntry, reqForm, next string) bool {
	if e.IsFallback == nil {
		return false
	}
	short, err := url.Parse(reqForm)
	if err != nil {
		return false
	}
	target, err := url.Parse(next)
	if err != nil {
		return false
	}
	return e.IsFallback(short, target)
}

// resolve runs the resolution loop for raw.
//
// The first clean comes first, so tracking is never sent. A skipped input,
// or one that matches no entry, is the first clean with no request. Then
// each hop: the URL in request form (https unless the entry allows http) is
// looked up in the Cache, else fetched by the flight call for its key,
// detached from ctx under its own Timeout. A registered next URL is
// requested as served; one that leaves the registry is cleaned once and,
// if the clean reveals another registered URL, the loop continues.
// ErrHopLimit, ErrLoop and ErrFallback are decided here on every call and
// never cached.
func (r *Resolver) resolve(ctx context.Context, raw string) chain {
	st := r.st
	off := r.c.Explain(raw)
	ch := chain{off: off, url: off.URL}
	if off.Skipped != linkclean.NotSkipped {
		return ch
	}
	e, pu, ok := st.lookup(off.URL, false)
	if !ok {
		return ch
	}
	fail := func(err error) chain {
		return chain{off: off, url: off.URL, err: err}
	}
	deadline := time.Now().Add(st.cfg.timeout)
	cur := off.URL
	seen := map[string]bool{seenForm(cur): true}
	allCached := true
	for hop := 0; ; hop++ {
		if hop > 0 {
			if e, pu, ok = st.lookup(cur, true); !ok {
				break
			}
		}
		host := pu.Host
		if hop == st.cfg.maxHops {
			return fail(rehydrate(CacheEntry{Failure: FailHopLimit}, e.Name, host, classNone, nil))
		}
		served := cur
		cur = requestForm(&pu, e.AllowHTTP)
		seen[cur] = true
		key := hopKey(e, st.cfg.policy, cur)

		h, hit := st.cache.Get(ctx, key)
		class := classNone
		if !hit {
			allCached = false
			if err := ctx.Err(); err != nil {
				return fail(callerGone(e, host, err))
			}
			wait := time.Until(deadline)
			if wait <= 0 {
				return fail(rehydrate(CacheEntry{Failure: FailTransient}, e.Name, host, classTimeout, nil))
			}
			// The call may outlive this caller: it gets copies, not the
			// loop's variables.
			entry, reqForm, hopIndex := e, cur, hop
			out, started := st.group.DoChan(key, host, func() (hopOutcome, func()) {
				return st.runHop(ctx, entry, hopIndex, reqForm, key)
			})
			if !started {
				return fail(rehydrate(CacheEntry{Failure: FailTransient}, e.Name, host, classInFlight, nil))
			}
			timer := time.NewTimer(wait)
			select {
			case o := <-out:
				timer.Stop()
				h, class = o.e, o.class
			case <-ctx.Done():
				timer.Stop()
				return fail(callerGone(e, host, ctx.Err()))
			case <-timer.C:
				return fail(rehydrate(CacheEntry{Failure: FailTransient}, e.Name, host, classTimeout, nil))
			}
		}

		if h.Failure == FailNone && h.Next == "" {
			// A stop: the strategy reported ErrNotShortLink.
			switch {
			case hop == 0:
				return chain{off: off, url: off.URL}
			case e.Intermediate:
				// Never return an intermediate redirector: fail open.
				return fail(rehydrate(h, e.Name, host, classNone, nil))
			}
			fin := r.c.Explain(served)
			ch.fins = append(ch.fins, finClean{after: len(ch.hops) - 1, res: fin})
			cur = fin.URL
			break
		}
		if h.Failure != FailNone {
			return fail(rehydrate(h, e.Name, host, class, nil))
		}
		next := h.Next
		if isFallback(e, cur, next) {
			return fail(rehydrate(CacheEntry{Failure: FailFallback}, e.Name, host, classNone, nil))
		}
		nf := seenForm(next)
		if seen[nf] {
			return fail(rehydrate(CacheEntry{Failure: FailLoop}, e.Name, host, classNone, nil))
		}
		seen[nf] = true
		ch.hops = append(ch.hops, Hop{From: cur, To: next, Entry: e.Name, Strategy: h.Strategy, Status: h.Status, Requests: h.Requests})
		if _, _, ok := st.lookup(next, true); ok {
			cur = next // a registered hop is requested as served, never cleaned first
			continue
		}
		fin := r.c.Explain(next)
		ch.fins = append(ch.fins, finClean{after: len(ch.hops) - 1, res: fin})
		cur = fin.URL
		seen[seenForm(cur)] = true
	}
	ch.url = cur
	ch.cached = allCached
	return ch
}

// callerGone is the error for a caller whose own ctx ended: transient,
// wrapping ctxErr.
func callerGone(e *compiledEntry, host string, ctxErr error) error {
	class := classDeadline
	if errors.Is(ctxErr, context.Canceled) {
		class = classCanceled
	}
	return rehydrate(CacheEntry{Failure: FailTransient}, e.Name, host, class, ctxErr)
}

// runHop is a hop's flight call. It runs detached from the caller that
// started it, under its own Timeout: it checks the Cache again (a call for
// the key may have completed since the caller's miss), else fetches the
// hop and stores a definitive outcome. The returned func emits the hop's
// RequestInfos once the key is released.
func (st *state) runHop(ctx context.Context, e *compiledEntry, hop int, reqForm, key string) (hopOutcome, func()) {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), st.cfg.timeout)
	defer cancel()
	if ce, ok := st.cache.Get(hctx, key); ok {
		return hopOutcome{e: ce, fromCache: true}, nil
	}
	o := st.fetchHop(hctx, e, hop, reqForm)
	st.store(hctx, e, key, reqForm, o.e)
	infos := o.infos
	o.infos = nil
	onRequest := st.cfg.onRequest
	if onRequest == nil || len(infos) == 0 {
		return o, nil
	}
	return o, func() {
		for _, info := range infos {
			emit(onRequest, info)
		}
	}
}

// emit calls onRequest, dropping a panic so later calls still happen.
func emit(onRequest func(RequestInfo), info RequestInfo) {
	defer func() { _ = recover() }()
	onRequest(info)
}

// store caches a hop outcome: never a transient one; a definitive failure
// for NegativeTTL; a next URL for PositiveTTL. A stop, or a next URL that
// is the entry's fallback, goes to NewMemoryCache's negative bound (still
// for PositiveTTL), so a flood of them cannot evict real successes.
func (st *state) store(ctx context.Context, e *compiledEntry, key, reqForm string, ce CacheEntry) {
	switch {
	case ce.Failure == FailTransient:
	case ce.Failure != FailNone:
		st.cache.Set(ctx, key, ce, st.cfg.negTTL)
	case ce.Next == "" || isFallback(e, reqForm, ce.Next):
		if ns, ok := st.cache.(negativeSetter); ok {
			ns.setNegative(ctx, key, ce, st.cfg.posTTL)
			return
		}
		st.cache.Set(ctx, key, ce, st.cfg.posTTL)
	default:
		st.cache.Set(ctx, key, ce, st.cfg.posTTL)
	}
}

// attempt is one strategy's try at a hop.
type attempt struct {
	strategy string
	kind     FailureKind
	class    errClass
	status   int    // from classifyStrategy
	next     string // validated; "" on a stop or a failure
	from, to int    // the Fetcher records this attempt made: recs[from:to]
}

// fetchHop fetches one hop with a fresh Fetcher (budget 2): e's Strategy,
// then its Fallback when that is eligible under the policy and the
// Strategy did not succeed. A validated next URL or ErrNotShortLink is a
// success. Otherwise the hop is transient if any attempt was, else the
// last attempt's kind. Status is the unexpected status for FailStatus and
// a transient HTTP status, else the status of the deciding attempt's last
// request if it did not read the body, else 0. Requests counts the request
// writes of both attempts.
func (st *state) fetchHop(ctx context.Context, e *compiledEntry, hop int, reqForm string) hopOutcome {
	u, err := url.Parse(reqForm)
	if err != nil {
		return hopOutcome{e: CacheEntry{Failure: FailBadTarget}, class: classBadTarget}
	}
	deadline, _ := ctx.Deadline()
	f := &Fetcher{st: st, e: e, hop: hop, hopCtx: ctx, deadline: deadline}
	defer f.finish() // also when a strategy panics
	primary, fallback := e.eligible(st.cfg.policy)
	var atts []attempt
	for _, s := range [...]Strategy{primary, fallback} {
		if s == nil {
			continue
		}
		a := attempt{strategy: s.Name(), from: f.recCount()}
		uc := *u // a strategy cannot change the next attempt's URL
		next, err := s.Next(ctx, f, &uc)
		a.kind, a.class, a.status = classifyStrategy(err)
		if a.kind == FailNone && err == nil {
			if v, ok := validateNext(next); ok {
				a.next = v
			} else {
				a.kind, a.class, a.status = FailBadTarget, classBadTarget, 0
			}
		}
		a.to = f.recCount()
		atts = append(atts, a)
		if a.kind == FailNone {
			break
		}
	}
	recs := f.finish()
	if len(atts) == 0 { // unreachable: lookup matched an eligible entry
		return hopOutcome{e: CacheEntry{Failure: FailTransient}, class: classOther}
	}

	dec := atts[len(atts)-1]
	if dec.kind != FailNone {
		for _, a := range atts {
			if a.kind == FailTransient {
				dec = a
				break
			}
		}
	}
	status := 0
	switch {
	case dec.kind == FailStatus, dec.kind == FailTransient:
		status = dec.status
	case dec.to > dec.from && dec.to <= len(recs) && !recs[dec.to-1].readBody:
		status = recs[dec.to-1].status
	}
	wire := 0
	infos := make([]RequestInfo, 0, len(recs))
	for i, rec := range recs {
		wire += rec.wire
		name := atts[len(atts)-1].strategy
		for _, a := range atts {
			if a.from <= i && i < a.to {
				name = a.strategy
				break
			}
		}
		infos = append(infos, RequestInfo{
			Entry: e.Name, Strategy: name, Host: rec.host, Hop: hop,
			Status: rec.status, Duration: rec.dur, Failure: rec.kind, Err: rec.err,
		})
	}
	return hopOutcome{
		e:     CacheEntry{Next: dec.next, Strategy: dec.strategy, Status: status, Requests: wire, Failure: dec.kind},
		class: dec.class,
		infos: infos,
	}
}

// recCount is the number of requests f has recorded.
func (f *Fetcher) recCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}
