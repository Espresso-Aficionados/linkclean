package unshorten

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A recorded fixture is one hop: testdata/responses/<host>/<case>.txtar.
// Its leading comment says where it came from (a live recording, a capture
// by hand, or "synthetic"). Its sections:
//
//	-- meta.json --  fixtureMeta
//	-- response --   a status line ("HTTP/1.1 301") and only the Location
//	                 and Content-Type headers
//	-- body --       empty, or an excerpt of the body (body strategies
//	                 only), which replay pads to body_len
//
// Tokens are TOKENn throughout; destinations are a maintainer-controlled
// target or example.net, with their shape kept.

// fixtureMeta is a fixture's meta.json.
type fixtureMeta struct {
	Entry    string `json:"entry"`    // the entry name, in DefaultRegistry or notRegisteredEntries
	Strategy string `json:"strategy"` // the strategy that served the hop
	Request  string `json:"request"`  // the hop URL, as the strategy got it
	Fetch    string `json:"fetch"`    // the URL the strategy GETs, when it is not Request
	Expect   struct {
		Next    string `json:"next"`    // the validated next URL; "" on failure
		Failure string `json:"failure"` // a FailureKind name
		Status  int    `json:"status"`  // Hop.Status: 0 for body and preview strategies
	} `json:"expect"`
	SetCookie bool   `json:"set_cookie"` // replay adds Set-Cookie: s=REDACTED
	Body      string `json:"body"`       // "none" or "excerpt"
	BodyLen   int    `json:"body_len"`   // the recorded body's length
}

type fixture struct {
	path    string
	comment string
	meta    fixtureMeta
	status  int
	header  http.Header
	body    []byte // as stored, before padding
}

// fetchURL is the URL the fixture's strategy requests.
func (fx *fixture) fetchURL() string {
	if fx.meta.Fetch != "" {
		return fx.meta.Fetch
	}
	return fx.meta.Request
}

// replayBody is the body replay serves: the excerpt padded with spaces to
// body_len.
func (fx *fixture) replayBody() []byte {
	b := bytes.Clone(fx.body)
	if n := fx.meta.BodyLen - len(b); n > 0 {
		b = append(b, bytes.Repeat([]byte(" "), n)...)
	}
	return b
}

// loadFixtures parses every fixture under testdata/responses.
func loadFixtures(tb testing.TB) []*fixture {
	tb.Helper()
	var out []*fixture
	err := filepath.WalkDir(filepath.Join("testdata", "responses"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(p, ".txtar") {
			tb.Errorf("%s: not a .txtar fixture", p)
			return nil
		}
		fx, err := parseFixture(p)
		if err != nil {
			tb.Errorf("%s: %v", p, err)
			return nil
		}
		out = append(out, fx)
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

func parseFixture(p string) (*fixture, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	comment, files := parseTxtar(data)
	fx := &fixture{path: p, comment: string(comment)}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"meta.json", "response", "body"}) {
		return nil, errors.New("sections are " + strings.Join(names, ", ") + "; want meta.json, response, body")
	}
	dec := json.NewDecoder(bytes.NewReader(files[0].Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx.meta); err != nil {
		return nil, err
	}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(bytes.Clone(files[1].Data), '\n'))))
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	proto, code, ok := strings.Cut(line, " ")
	if fx.status, err = strconv.Atoi(code); !ok || proto != "HTTP/1.1" || err != nil {
		return nil, errors.New("bad status line " + strconv.Quote(line))
	}
	mh, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	fx.header = http.Header(mh)
	fx.body = files[2].Data
	return fx, nil
}

// fixtureEntries are the entries fixtures may name.
func fixtureEntries(tb testing.TB) *Registry {
	r, err := DefaultRegistry().With(notRegisteredEntries()...)
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

var (
	tokenPath   = regexp.MustCompile(`^/TOKEN[0-9]+\+?/?$`)
	anyToken    = regexp.MustCompile(`TOKEN[0-9]+`)
	provenances = regexp.MustCompile(`(?m)^# \S+ \S+\. (recorded [0-9]{4}-[0-9]{2}-[0-9]{2} livenet|capture [0-9]{4}-[0-9]{2}-[0-9]{2}|synthetic)`)
)

// TestFixtureFormat checks every fixture against the format, its entry and
// the scrubbing rules that can be checked mechanically.
func TestFixtureFormat(t *testing.T) {
	reg := fixtureEntries(t)
	fxs := loadFixtures(t)
	if len(fxs) == 0 {
		t.Fatal("no fixtures")
	}
	seen := map[string]bool{}
	for _, fx := range fxs {
		m := fx.meta
		seen[filepath.ToSlash(fx.path)] = true
		if !provenances.MatchString(fx.comment) {
			t.Errorf("%s: comment does not start with its host, case and provenance", fx.path)
		}
		e := reg.entry(m.Entry)
		if e == nil {
			t.Errorf("%s: unknown entry %q", fx.path, m.Entry)
			continue
		}
		if m.Strategy != name(e.Strategy) && m.Strategy != name(e.Fallback) {
			t.Errorf("%s: strategy %q is not %s's", fx.path, m.Strategy, m.Entry)
		}
		req, err := url.Parse(m.Request)
		if err != nil || req.Scheme != "https" || filepath.Base(filepath.Dir(fx.path)) != req.Host {
			t.Errorf("%s: request %q is not an https URL on the directory's host", fx.path, m.Request)
		}
		if err == nil && (!tokenPath.MatchString(req.EscapedPath()) || req.RawQuery != "" || req.Fragment != "") {
			t.Errorf("%s: request %q: the short code must be TOKENn", fx.path, m.Request)
		}
		if _, ok := failureByName(m.Expect.Failure); !ok {
			t.Errorf("%s: failure %q is not a FailureKind name", fx.path, m.Expect.Failure)
		}
		if (m.Expect.Failure == "none") != (m.Expect.Next != "") {
			t.Errorf("%s: next %q with failure %q", fx.path, m.Expect.Next, m.Expect.Failure)
		}
		bodyStrategy := m.Strategy != "Redirect"
		switch {
		case m.Body != "none" && m.Body != "excerpt":
			t.Errorf("%s: body %q", fx.path, m.Body)
		case (m.Body == "excerpt") != bodyStrategy:
			t.Errorf("%s: body %q for strategy %s: bodies are kept for body strategies only", fx.path, m.Body, m.Strategy)
		case m.Body == "none" && len(fx.body) != 0:
			t.Errorf("%s: body \"none\" but the body section is not empty", fx.path)
		case len(fx.body) > m.BodyLen:
			t.Errorf("%s: body_len %d is shorter than the excerpt (%d)", fx.path, m.BodyLen, len(fx.body))
		}
		if bodyStrategy && m.Expect.Status != 0 {
			t.Errorf("%s: status %d for a body strategy, want 0", fx.path, m.Expect.Status)
		}
		for k := range fx.header {
			if k != "Location" && k != "Content-Type" {
				t.Errorf("%s: header %s kept; only Location and Content-Type are", fx.path, k)
			}
		}
		raw, _ := os.ReadFile(fx.path)
		for _, bad := range []string{"/Users/", "/private/", "/home/", "Set-Cookie:", "Authorization"} {
			if bytes.Contains(raw, []byte(bad)) {
				t.Errorf("%s: contains %q", fx.path, bad)
			}
		}
		if !anyToken.Match(raw) {
			t.Errorf("%s: no TOKENn", fx.path)
		}
	}
	// The fixtures the tests name.
	for _, p := range []string{"testdata/responses/amzn.to/amzn_to_affiliate.txtar"} {
		if !seen[p] {
			t.Errorf("missing fixture %s", p)
		}
	}
}

func failureByName(s string) (FailureKind, bool) {
	for k := FailNone; k <= FailTransient; k++ {
		if k.String() == s {
			return k, true
		}
	}
	return 0, false
}

// TestFixtureStrategies replays each fixture's response to its entry's
// strategy over an opaque transport and checks meta.expect: the strategy
// must request exactly the fetch URL and produce the recorded outcome, so
// scrubbing cannot hide extraction drift. (TestRecordedFixtures replays the
// same fixtures through the test network and the real guard.)
func TestFixtureStrategies(t *testing.T) {
	all := fixtureEntries(t)
	for _, fx := range loadFixtures(t) {
		t.Run(filepath.ToSlash(strings.TrimPrefix(fx.path, filepath.Join("testdata", "responses")+string(filepath.Separator))), func(t *testing.T) {
			e := all.entry(fx.meta.Entry)
			if e == nil {
				t.Fatalf("unknown entry %q", fx.meta.Entry)
			}
			s := e.Strategy
			if fx.meta.Strategy != name(s) {
				s = e.Fallback
			}
			reg, err := NewRegistry(e.Entry)
			if err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				var reqs []string
				rt := rtFunc(func(req *http.Request) (*http.Response, error) {
					reqs = append(reqs, req.URL.String())
					if req.URL.String() != fx.fetchURL() {
						return response(req, 404, nil, ""), nil
					}
					h := fx.header.Clone()
					if fx.meta.SetCookie {
						h.Set("Set-Cookie", "s=REDACTED; Path=/")
					}
					return response(req, fx.status, h, string(fx.replayBody())), nil
				})
				st := newTestState(t, Config{}, reg, nil, rt)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				f := newTestFetcher(t, st, e.Name, ctx)
				next, err := s.Next(ctx, f, mustURL(t, fx.meta.Request))
				recs := f.finish()
				if len(reqs) != 1 || reqs[0] != fx.fetchURL() {
					t.Errorf("requests %q, want only %s", reqs, fx.fetchURL())
				}
				checkFixtureOutcome(t, fx, next, err, recs)
			})
		})
	}
}

// checkFixtureOutcome checks a strategy's result for fx against
// meta.expect: the failure kind, the validated next URL, and the status by
// the Hop.Status rule (the last request's status unless it read the body;
// the unexpected status for FailStatus).
func checkFixtureOutcome(t *testing.T, fx *fixture, next *url.URL, err error, recs []requestRecord) {
	t.Helper()
	kind := kindOf(err)
	got := ""
	if next != nil {
		got = next.String()
		if v, ok := validateNext(next); !ok {
			kind = FailBadTarget
		} else {
			got = v
		}
	}
	if want, _ := failureByName(fx.meta.Expect.Failure); kind != want || got != fx.meta.Expect.Next {
		t.Errorf("outcome %s %q (%v), want %s %q", kind, got, err, fx.meta.Expect.Failure, fx.meta.Expect.Next)
	}
	status := 0
	if n := len(recs); n > 0 && !recs[n-1].readBody {
		status = recs[n-1].status
	}
	if kind == FailStatus {
		var se *StatusError
		errors.As(err, &se)
		status = se.Status
	}
	if status != fx.meta.Expect.Status {
		t.Errorf("status %d, want %d", status, fx.meta.Expect.Status)
	}
}

// kindOf maps a strategy error to its FailureKind by the package's
// sentinels, as classifyStrategy does for these cases.
func kindOf(err error) FailureKind {
	var se *StatusError
	switch {
	case err == nil, errors.Is(err, ErrNotShortLink):
		return FailNone
	case errors.As(err, &se):
		if transientStatus(se.Status) {
			return FailTransient
		}
		return FailStatus
	}
	for _, k := range []FailureKind{FailNoRedirect, FailNoMatch, FailBadTarget, FailFetchDenied, FailBudget} {
		if errors.Is(err, kindSentinel(k)) {
			return k
		}
	}
	return FailTransient
}
