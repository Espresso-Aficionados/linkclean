//go:build livenet

package unshorten_test

// The live drift checks: TestLive sends real requests to the shorteners in
// a links file and checks each link's outcome against the file's
// expectations. Only the maintainer runs it, from the workstation, with the
// commands in testdata/livenet/README.md. It never runs in CI (CI only
// compiles it, with go vet -tags livenet), and an agent never runs it.
//
// Check mode uses the production path: New with the default guarded
// transport, the default cache, NewHostLimiter(1, 1) and an OnRequest
// collector. Record mode wraps NewTransport() in a recording RoundTripper
// (AllowUnguardedTransport; the dial guard is still in the dial path) and a
// cache that stores nothing but records every hop outcome, so each request
// becomes a scrubbed fixture draft (scrub_test.go). A draft is written only
// when no refusal fires and a replay of the drafts through the real
// strategies stores the same hop outcomes; otherwise nothing of that link is
// written.
//
// Both modes write a results file from testdata/livenet/results.md.tmpl
// into -livenet.out, which must be an existing 0700 directory outside every
// git work tree. Nothing is written into the repository.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
	"github.com/Espresso-Aficionados/linkclean/unshorten"
	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

var (
	flagLinks     = flag.String("livenet.links", "", "the links file, outside every git work tree; without it TestLive skips")
	flagOut       = flag.String("livenet.out", "", "absolute path of an existing 0700 directory outside every git work tree, for the results and drafts")
	flagRecord    = flag.Bool("livenet.record", false, "record mode: also write scrubbed fixture drafts to <out>/drafts")
	flagDeny      = flag.String("livenet.deny", "", "the deny list (one string per line), outside every git work tree; required with -livenet.record")
	flagMarketing = flag.Bool("livenet.marketing", false, "register tinyurl and bit.ly, which DefaultRegistry does not, for this run")
	flagCommit    = flag.String("livenet.commit", "", "the commit under test, for the results file (git rev-parse HEAD)")
)

var commitHash = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// liveTimeout is the Resolver's Timeout in both modes (the default).
const liveTimeout = 6 * time.Second

func TestLive(t *testing.T) {
	if *flagLinks == "" {
		t.Skip("no -livenet.links: the live suite runs only with the maintainer's links file (testdata/livenet/README.md)")
	}
	if run := flag.Lookup("test.run"); run == nil || run.Value.String() != "^TestLive$" {
		t.Fatal("run the live suite alone, with -run '^TestLive$': it lifts the test binary's network seal for its own duration")
	}
	if *flagCommit != "" && !commitHash.MatchString(*flagCommit) {
		t.Fatal("-livenet.commit is not a commit hash")
	}
	if err := checkOutDir(*flagOut); err != nil {
		t.Fatal(err)
	}
	raw, err := readInput("livenet.links", *flagLinks)
	if err != nil {
		t.Fatal(err)
	}
	lf, err := parseLinks(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var deny []string
	switch {
	case *flagDeny != "":
		b, err := readInput("livenet.deny", *flagDeny)
		if err != nil {
			t.Fatal(err)
		}
		if deny, err = parseDeny(b); err != nil {
			t.Fatal(err)
		}
	case *flagRecord:
		t.Fatal("-livenet.record needs -livenet.deny: the account names, IDs and tags the drafts must never hold")
	}
	tmpl, err := os.ReadFile(liveTemplatePath)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := liveRegistry(*flagMarketing)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := gateEntries(reg.Entries())
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]bool{}
	for _, e := range entries {
		for _, h := range e.Hosts {
			hosts[h] = true
		}
	}
	date := time.Now().UTC().Format(time.DateOnly)
	mode, name := "check", "results-"+date+".md"
	if *flagRecord {
		mode, name = "record", "record-"+date+".md"
		if _, err := os.Lstat(filepath.Join(*flagOut, "drafts")); err == nil {
			t.Fatal("-livenet.out already holds drafts/; record into a fresh directory")
		}
	}
	if _, err := os.Lstat(filepath.Join(*flagOut, name)); err == nil {
		t.Fatalf("-livenet.out already holds %s; use a fresh directory", name)
	}

	// TestMain seals the binary off the network with a fail-closed Resolve
	// seam; lift it for this test only. AllowAddr stays unset, so every dial
	// meets the real guard.
	t.Cleanup(dialhook.SetResolve(nil))

	lr := &liveRun{
		t: t, record: *flagRecord, reg: reg, hosts: hosts, deny: deny, out: *flagOut, date: date,
		lim: unshorten.NewHostLimiter(1, 1), resolvers: map[unshorten.GenericPolicy]*unshorten.Resolver{},
	}
	if lr.record {
		lr.rec = &recorder{rt: unshorten.NewTransport(unshorten.TransportOptions{})}
		lr.spy = &spyCache{}
	}
	res := liveResults{
		Mode: mode, Date: date, Egress: liveEgress, Commit: *flagCommit, GoVersion: runtime.Version(),
		UserAgent: liveUA(), Marketing: *flagMarketing,
	}
	for _, l := range lf.Links {
		c := lr.one(l)
		if c.Pass {
			res.Passed++
		} else {
			res.Failed++
		}
		res.Cases = append(res.Cases, c)
	}
	res.Gating = gate(entries, res.Cases)
	b, err := renderResults(string(tmpl), res)
	if err != nil {
		t.Fatal(err)
	}
	if p := refusals(string(b), deny, linkOriginals(lf)); len(p) > 0 {
		t.Fatalf("the results file was not written: %s", strings.Join(p, "; "))
	}
	if err := writeNew(filepath.Join(*flagOut, name), b); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	t.Logf("wrote %s: %d cases, %d passed, %d failed", name, len(res.Cases), res.Passed, res.Failed)
	if res.Failed > 0 {
		t.Errorf("%d of %d cases failed; see %s", res.Failed, len(res.Cases), name)
	}
}

// liveRegistry is DefaultRegistry, plus tinyurl and bit.ly (the entries
// doc.go's Not registered by default shows) when
// marketing is set and DefaultRegistry lacks them.
func liveRegistry(marketing bool) (*unshorten.Registry, error) {
	reg := unshorten.DefaultRegistry()
	if !marketing {
		return reg, nil
	}
	have := map[string]bool{}
	for _, e := range reg.Entries() {
		have[e.Name] = true
	}
	var extra []unshorten.Entry
	if !have["tinyurl"] {
		extra = append(extra, unshorten.Entry{
			Name: "tinyurl", Match: []string{"*://tinyurl.com/*"},
			Strategy: unshorten.TinyURLPreview(), Fallback: unshorten.Redirect(), Kind: unshorten.Marketing,
		})
	}
	if !have["bit.ly"] {
		extra = append(extra, unshorten.Entry{
			Name: "bit.ly", Match: []string{"*://bit.ly/*", "*://bitly.com/*"},
			Strategy: unshorten.BitlyInfo(), Fallback: unshorten.Redirect(), Kind: unshorten.Marketing,
			IsFallback: bitlyRoot,
		})
	}
	if len(extra) == 0 {
		return reg, nil
	}
	return reg.With(extra...)
}

// bitlyRoot is bit.ly's IsFallback: a bitly.com or bit.ly root.
func bitlyRoot(_, target *url.URL) bool {
	h := strings.ToLower(target.Hostname())
	site := h == "bit.ly" || h == "bitly.com" || strings.HasSuffix(h, ".bitly.com")
	return site && (target.Path == "" || target.Path == "/") && target.RawQuery == ""
}

// liveUA is the product token of the default User-Agent as the test binary
// builds it.
func liveUA() string {
	v := "devel"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path == "github.com/Espresso-Aficionados/linkclean" &&
		bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = bi.Main.Version
	}
	return "linkclean/" + v
}

type liveRun struct {
	t         *testing.T
	record    bool
	reg       *unshorten.Registry
	hosts     map[string]bool
	deny      []string
	out, date string
	lim       unshorten.Limiter // shared: one rate budget per host for the whole run
	resolvers map[unshorten.GenericPolicy]*unshorten.Resolver
	infos     infoLog
	rec       *recorder // record mode
	spy       *spyCache // record mode
}

func (lr *liveRun) resolver(p unshorten.GenericPolicy) *unshorten.Resolver {
	if r, ok := lr.resolvers[p]; ok {
		return r
	}
	cfg := unshorten.Config{Registry: lr.reg, Generic: p, Limiter: lr.lim, Timeout: liveTimeout, OnRequest: lr.infos.add}
	if lr.record {
		cfg.Transport, cfg.AllowUnguardedTransport, cfg.Cache = lr.rec, true, lr.spy
	}
	r, err := unshorten.New(cfg)
	if err != nil {
		lr.t.Fatalf("New: %v", err)
	}
	lr.resolvers[p] = r
	return r
}

// one resolves l and checks it; in record mode it also drafts its fixtures.
func (lr *liveRun) one(l liveLink) caseResult {
	p, _ := policyOf(l.Generic)
	u, _ := url.Parse(l.URL)
	c := caseResult{Case: u.Hostname() + "/" + l.Case, Kind: l.Kind, Origin: l.Origin, Policy: []string{"PreviewOnly", "Follow", "Skip"}[p]}
	r := lr.resolver(p)
	entry, _ := r.Match(l.URL)
	start := time.Now()
	res, err := r.Resolve(context.Background(), l.URL)
	fail := failureOf(err)
	infos := lr.settle(start, fail == unshorten.FailTransient.String())
	var xs []*rawExchange
	var sets []spySet
	if lr.record {
		xs, sets = lr.rec.take(), lr.spy.take()
	}

	cl := linkclean.Default()
	o := observed{Entry: entry, URL: res.URL, Clean: cl.Clean(l.URL), Fixed: cl.Clean(res.URL) == res.URL, Failure: fail}
	for _, h := range res.Hops {
		o.Hops = append(o.Hops, liveHop{h.Entry, h.Strategy, h.Status})
	}
	c.Entries = []string{entry}
	for _, ri := range infos {
		o.Requests = append(o.Requests, observedRequest{ri.Entry, ri.Strategy, ri.Host, ri.Status})
		c.Entries = append(c.Entries, ri.Entry)
		if ri.Status != 0 {
			c.Hosts = append(c.Hosts, ri.Host)
		}
	}
	c.Problems = evaluate(l, o)
	c.Pass = len(c.Problems) == 0
	c.Entry, c.Outcome = entry, fail
	if n := len(infos); n > 0 {
		c.Strategy, c.Status = infos[n-1].Strategy, infos[n-1].Status
	}
	if fu, err := url.Parse(res.URL); err == nil {
		c.DestHost = fu.Hostname()
	}
	if lr.record {
		c.Drafts = lr.draft(l, infos, xs, sets)
	}
	verdict := "pass"
	if !c.Pass {
		verdict = "FAIL: " + strings.Join(c.Problems, "; ")
	}
	if lr.record {
		verdict += "; drafts " + c.Drafts
	}
	lr.t.Logf("%s: %s (outcome %s, %d requests)", c.Case, verdict, fail, len(infos))
	return c
}

// settle waits for the link's last OnRequest calls, which run after the
// hop's lookup is released and so may follow Resolve's return. A transient
// outcome may have left a detached hop running: it ends within Timeout of
// its start, which is within Timeout of Resolve's start.
func (lr *liveRun) settle(start time.Time, transient bool) []unshorten.RequestInfo {
	until := time.Now().Add(300 * time.Millisecond)
	if transient {
		until = start.Add(2*liveTimeout + time.Second)
	}
	time.Sleep(time.Until(until))
	if lr.record && !lr.rec.waitIdle(5*time.Second) {
		lr.t.Log("a response body was still open after the hop deadline")
	}
	return lr.infos.take()
}

// draft builds, checks, replays and writes the drafts of one link, and
// returns what happened, for the results. It never returns content.
func (lr *liveRun) draft(l liveLink, infos []unshorten.RequestInfo, xs []*rawExchange, sets []spySet) string {
	switch {
	case l.Origin == "public":
		return "none: a public link is checked, never recorded"
	case len(xs) == 0:
		return "none: no request"
	case len(xs) != len(infos):
		return "refused: the recorder and OnRequest saw different request counts"
	case !lr.rec.idle():
		return "refused: a response body was never closed"
	}
	c := liveCapture{Case: l.Case, Date: lr.date, UA: xs[0].ua, Input: l.URL}
	for i, x := range xs {
		ri := infos[i]
		if u, err := url.Parse(x.request); err != nil || !strings.EqualFold(u.Hostname(), ri.Host) {
			return "refused: the recorder and OnRequest disagree on a request's host"
		}
		c.Exchanges = append(c.Exchanges, liveExchange{
			Entry: ri.Entry, Strategy: ri.Strategy, Failure: ri.Failure.String(), Hop: ri.Hop,
			Request: x.request, Proto: x.proto, Status: x.status, Location: x.location, ContentType: x.contentType,
			SetCookie: x.setCookie, Body: x.body, BodyRead: x.bodyRead, BodyLen: max(len(x.body), int(x.contentLength)),
		})
	}
	for k, s := range sets {
		name, _, _ := strings.Cut(s.key, "\x00")
		for _, x := range c.Exchanges {
			if x.Hop == k && x.Entry != name {
				return "refused: the stored hop outcomes do not line up with the requests"
			}
		}
		c.Hops = append(c.Hops, liveOutcome{Next: s.e.Next, Strategy: s.e.Strategy, Status: s.e.Status, Failure: s.e.Failure.String()})
	}
	sl, err := scrubCapture(c, lr.hosts, l.KeepDestination, lr.deny)
	if err != nil {
		if msg := err.Error(); strings.HasPrefix(msg, "refused: ") {
			return msg
		}
		return "refused: " + err.Error()
	}
	if err := lr.replay(l, sl); err != nil {
		return "refused: replay: " + err.Error()
	}
	var written []string
	for _, f := range sl.Files {
		p := filepath.Join(lr.out, "drafts", filepath.FromSlash(f.Path))
		if err := writeNew(p, f.Data); err != nil {
			for _, w := range written {
				os.Remove(w)
			}
			lr.t.Logf("writing a draft of %s: %v", l.Case, err)
			return "refused: a draft could not be written"
		}
		written = append(written, p)
	}
	var names []string
	for _, f := range sl.Files {
		names = append(names, f.Path)
	}
	return "written: " + strings.Join(names, ", ")
}

// replay resolves the scrubbed link through the real strategies against the
// drafts as written, and requires the same hop outcomes as the live run.
func (lr *liveRun) replay(l liveLink, sl scrubbedLink) error {
	rt := replayTransport{}
	for _, f := range sl.Files {
		d, err := parseDraft(f.Data)
		if err != nil {
			return err
		}
		if _, dup := rt[d.Meta.Request]; dup {
			return errors.New("two drafts for one request URL")
		}
		rt[d.Meta.Request] = d
	}
	p, _ := policyOf(l.Generic)
	spy := &spyCache{}
	r, err := unshorten.New(unshorten.Config{
		Transport: rt, AllowUnguardedTransport: true, Registry: lr.reg, Generic: p, Cache: spy,
		Limiter: unshorten.NoLimit(), Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return err
	}
	_, _ = r.Resolve(context.Background(), sl.Input) // the outcome is in spy
	got := spy.take()
	if len(got) != len(sl.Hops) {
		return fmt.Errorf("the replay stored %d hop outcomes, the live run %d", len(got), len(sl.Hops))
	}
	for k, g := range got {
		w := sl.Hops[k]
		for _, d := range []struct {
			field string
			ok    bool
		}{
			{"next", g.e.Next == w.Next}, {"strategy", g.e.Strategy == w.Strategy},
			{"status", g.e.Status == w.Status}, {"failure", g.e.Failure.String() == w.Failure},
		} {
			if !d.ok {
				return fmt.Errorf("hop %d's %s differs from the live run's", k, d.field)
			}
		}
	}
	return nil
}

// replayTransport answers each request with its draft.
type replayTransport map[string]*draft

func (rt replayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d, ok := rt[req.URL.String()]
	if !ok {
		return nil, errors.New("replay: no draft for the request")
	}
	h := http.Header{}
	if d.Location != "" {
		h.Set("Location", d.Location)
	}
	if d.ContentType != "" {
		h.Set("Content-Type", d.ContentType)
	}
	return &http.Response{
		Status: fmt.Sprintf("%d %s", d.Status, http.StatusText(d.Status)), StatusCode: d.Status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: h,
		Body: io.NopCloser(bytes.NewReader(d.Body)), ContentLength: int64(len(d.Body)), Request: req,
	}, nil
}

// infoLog collects RequestInfos from OnRequest.
type infoLog struct {
	mu    sync.Mutex
	infos []unshorten.RequestInfo
}

func (l *infoLog) add(ri unshorten.RequestInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, ri)
}

func (l *infoLog) take() []unshorten.RequestInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.infos
	l.infos = nil
	return out
}

// spyCache stores nothing, so every hop is fetched, and records every hop
// outcome the Resolver stores (every one but a transient one).
type spyCache struct {
	mu   sync.Mutex
	sets []spySet
}

type spySet struct {
	key string // entry NUL strategies NUL request form
	e   unshorten.CacheEntry
}

func (c *spyCache) Get(context.Context, string) (unshorten.CacheEntry, bool) {
	return unshorten.CacheEntry{}, false
}

func (c *spyCache) Set(_ context.Context, key string, e unshorten.CacheEntry, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets = append(c.sets, spySet{key, e})
}

func (c *spyCache) take() []spySet {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sets
	c.sets = nil
	return out
}

// recordCap bounds a recorded body: the Fetcher reads at most 64 KiB.
const recordCap = 64<<10 + 1

// recorder is record mode's RoundTripper. It keeps, per request, the URL,
// the status, Location, Content-Type, whether Set-Cookie was present (never
// its value), and the body bytes the Fetcher read. All of it stays in
// memory until the scrubber has passed it.
type recorder struct {
	rt     http.RoundTripper
	mu     sync.Mutex
	xs     []*rawExchange
	active int // requests whose response body is not closed yet
}

type rawExchange struct {
	request, ua, proto, location, contentType string
	status                                    int
	setCookie, bodyRead                       bool
	body                                      []byte
	contentLength                             int64
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	x := &rawExchange{request: req.URL.String(), ua: req.Header.Get("User-Agent")}
	r.mu.Lock()
	r.xs = append(r.xs, x)
	r.active++
	r.mu.Unlock()
	resp, err := r.rt.RoundTrip(req)
	if err != nil {
		r.done()
		return nil, err
	}
	x.status, x.proto = resp.StatusCode, resp.Proto
	x.location, x.contentType = resp.Header.Get("Location"), resp.Header.Get("Content-Type")
	x.setCookie = len(resp.Header.Values("Set-Cookie")) > 0
	x.contentLength = resp.ContentLength
	resp.Body = &teeBody{ReadCloser: resp.Body, x: x, r: r}
	return resp, nil
}

func (r *recorder) done() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
}

func (r *recorder) idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active == 0
}

func (r *recorder) waitIdle(d time.Duration) bool {
	for end := time.Now().Add(d); !r.idle(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			return false
		}
	}
	return true
}

// take returns the exchanges so far and forgets them. Call it only when
// idle: the exchanges' fields are written before their body is closed.
func (r *recorder) take() []*rawExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.xs
	r.xs = nil
	return slices.Clip(out)
}

type teeBody struct {
	io.ReadCloser
	x    *rawExchange
	r    *recorder
	once sync.Once
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.x.bodyRead = true
	if room := recordCap - len(b.x.body); room > 0 {
		b.x.body = append(b.x.body, p[:min(n, room)]...)
	}
	return n, err
}

func (b *teeBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.r.done)
	return err
}
