package unshorten

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// served is one canned response, keyed by the request URL.
type served struct {
	status   int
	location string
	body     string
}

// runStrategy runs s.Next for u under entry over an opaque transport
// serving pages, inside a synctest bubble. It returns the next URL ("" on
// error), the error and the request URLs in order.
func runStrategy(t *testing.T, entry Entry, s Strategy, u string, pages map[string]served) (next string, err error, reqs []string) {
	t.Helper()
	reg, rerr := NewRegistry(entry)
	if rerr != nil {
		t.Fatal(rerr)
	}
	synctest.Test(t, func(t *testing.T) {
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			reqs = append(reqs, req.URL.String())
			p, ok := pages[req.URL.String()]
			if !ok {
				return response(req, 404, nil, "not found"), nil
			}
			h := http.Header{"Content-Type": {"text/html"}}
			if p.location != "" {
				h.Set("Location", p.location)
			}
			return response(req, p.status, h, p.body), nil
		})
		st := newTestState(t, Config{}, reg, nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, entry.Name, ctx)
		n, e := s.Next(ctx, f, mustURL(t, u))
		f.finish()
		if n != nil {
			next = n.String()
		}
		err = e
	})
	return next, err, reqs
}

func TestStrategyNames(t *testing.T) {
	for s, want := range map[Strategy]string{
		Redirect(): "Redirect", BodyRegexp(regexp.MustCompile(`(x)`)): "BodyRegexp", TikTokOEmbed(): "TikTokOEmbed",
		TinyURLPreview(): "TinyURLPreview", BitlyInfo(): "BitlyInfo",
	} {
		if s.Name() != want {
			t.Errorf("Name() = %q, want %q", s.Name(), want)
		}
	}
}

func TestRedirect(t *testing.T) {
	entry := Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: Redirect()}
	for _, status := range []int{200, 204, 300, 301, 302, 303, 304, 305, 307, 308, 400, 403, 404, 410, 429, 500, 501, 502, 503, 504, 999} {
		for _, loc := range []string{"", "https://example.net/p?utm_source=x"} {
			next, err, reqs := runStrategy(t, entry, Redirect(), "https://t.co/TOKEN1#f",
				map[string]served{"https://t.co/TOKEN1": {status: status, location: loc}})
			if len(reqs) != 1 {
				t.Errorf("%d %q: %d requests", status, loc, len(reqs))
			}
			redirect := status == 301 || status == 302 || status == 303 || status == 307 || status == 308
			var se *StatusError
			switch {
			case redirect && loc != "":
				if err != nil || next != loc {
					t.Errorf("%d %q: %q, %v", status, loc, next, err)
				}
			case transientStatus(status):
				if !errors.As(err, &se) || se.Status != status || !errors.Is(err, ErrUnresolved) {
					t.Errorf("%d %q: err %v, want a StatusError", status, loc, err)
				}
			default:
				if !errors.Is(err, ErrNoRedirect) || next != "" {
					t.Errorf("%d %q: %q, %v; want ErrNoRedirect", status, loc, next, err)
				}
			}
		}
	}
}

func TestBodyRegexp(t *testing.T) {
	re := regexp.MustCompile(`<a class="go" href="([^"]*)"`)
	entry := Entry{Name: "lnkd.in", Match: []string{"*://lnkd.in/*"}, Strategy: BodyRegexp(re)}
	tests := []struct {
		status int
		body   string
		want   string
		err    error
		code   int // StatusError status
	}{
		{200, `<a class="go" href="https://example.net/p?a=1&amp;b=2">`, "https://example.net/p?a=1&b=2", nil, 0},
		{200, `<a class="go" href="/rel">`, "https://lnkd.in/rel", nil, 0},
		{200, `<a class="go" href="//example.net/p">`, "https://example.net/p", nil, 0},
		{200, `x <a class="go" href="https://a.example/"> <a class="go" href="https://b.example/">`, "https://a.example/", nil, 0},
		{200, `<a class="go" href="">`, "", ErrNoMatch, 0},
		{200, `<a class="other" href="https://example.net/">`, "", ErrNoMatch, 0},
		{200, `<a class="go" href="http://[::1">`, "", ErrBadTarget, 0},
		{999, `<a class="go" href="https://example.net/">`, "", nil, 999},
		{404, ``, "", nil, 404},
		{503, ``, "", nil, 503},
		{301, ``, "", nil, 301},
	}
	for _, tt := range tests {
		next, err, _ := runStrategy(t, entry, entry.Strategy, "https://lnkd.in/TOKEN1",
			map[string]served{"https://lnkd.in/TOKEN1": {status: tt.status, body: tt.body}})
		var se *StatusError
		switch {
		case tt.code != 0:
			if !errors.As(err, &se) || se.Status != tt.code {
				t.Errorf("%d %q: err %v, want status %d", tt.status, tt.body, err, tt.code)
			}
		case next != tt.want || !errors.Is(err, tt.err) || (err == nil) != (tt.err == nil):
			t.Errorf("%d %q: %q, %v; want %q, %v", tt.status, tt.body, next, err, tt.want, tt.err)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("BodyRegexp without a capturing group did not panic")
		}
	}()
	BodyRegexp(regexp.MustCompile(`no group`))
}

func TestTikTokOEmbed(t *testing.T) {
	entry := Entry{Name: "tiktok", Match: []string{"*://vm.tiktok.com/*"}, FetchHosts: []string{"www.tiktok.com"},
		Strategy: TikTokOEmbed()}
	const oembed = "https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FTOKEN1%2F"
	html := func(cite string) string {
		return strconv.Quote(`<blockquote class="tiktok-embed" cite="` + cite + `" data-video-id="123"><section></section></blockquote>`)
	}
	const video = "https://www.tiktok.com/@user.name_1/video/123"
	tests := []struct {
		status int
		body   string
		want   string
		err    error
	}{
		{200, `{"html":` + html(video) + `,"embed_product_id":"123"}`, video, nil},
		{200, `{"html":` + html(video) + `,"embed_product_id":123}`, video, nil},
		{200, `{"html":` + html(video) + `}`, video, nil},
		{200, `{"html":` + html(video) + `,"embed_product_id":null}`, video, nil},
		{200, `{"html":` + html("https://www.tiktok.com/@u/photo/77") + `,"embed_product_id":"77"}`, "https://www.tiktok.com/@u/photo/77", nil},
		{200, `{"html":` + html(video) + `,"embed_product_id":"124"}`, "", ErrNoMatch},
		{200, `{"html":` + html(video) + `,"embed_product_id":true}`, "", ErrNoMatch},
		{200, `{"html":` + html("https://www.tiktok.com/@u/video/123?x=1") + `}`, "", ErrNoMatch},
		{200, `{"html":` + html("https://evil.example/@u/video/123") + `}`, "", ErrNoMatch},
		{200, `{"html":` + html("https://www.tiktok.com/@u/live/123") + `}`, "", ErrNoMatch},
		{200, `{"html":` + html("http://www.tiktok.com/@u/video/123") + `}`, "", ErrNoMatch},
		{200, `{"html":"<p>no quote</p>"}`, "", ErrNoMatch},
		{200, `{"title":"x"}`, "", ErrNoMatch},
		{200, `not json`, "", ErrNoMatch},
		{200, `{"html":` + html(video), "", ErrNoMatch},
		{400, `{}`, "", nil},
	}
	for _, tt := range tests {
		next, err, reqs := runStrategy(t, entry, entry.Strategy, "https://vm.tiktok.com/TOKEN1/",
			map[string]served{oembed: {status: tt.status, body: tt.body}})
		if len(reqs) != 1 || reqs[0] != oembed {
			t.Errorf("requests %q, want only %s", reqs, oembed)
		}
		if tt.status != 200 {
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tt.status {
				t.Errorf("%d: err %v", tt.status, err)
			}
			continue
		}
		if next != tt.want || !errors.Is(err, tt.err) || (err == nil) != (tt.err == nil) {
			t.Errorf("%s: %q, %v; want %q, %v", tt.body, next, err, tt.want, tt.err)
		}
	}
}

func TestTinyURLPreview(t *testing.T) {
	entry := Entry{Name: "tinyurl", Match: []string{"*://tinyurl.com/*"}, Strategy: TinyURLPreview(), Kind: Marketing}
	page := func(href string) string {
		return `<a id="long-link" href="` + href + `"
                        class="text-3xl" target="_blank">x</a>`
	}
	tests := []struct {
		in, fetch string
		body      string
		want      string
		err       error
	}{
		{"https://tinyurl.com/TOKEN1", "https://tinyurl.com/preview/TOKEN1", page("https://www.example.com"), "https://www.example.com", nil},
		{"https://tinyurl.com/TOKEN1/extra?x=1#f", "https://tinyurl.com/preview/TOKEN1", page("https://example.net/p?a=1&amp;b=2"), "https://example.net/p?a=1&b=2", nil},
		{"https://tinyurl.com/TOKEN1", "https://tinyurl.com/preview/TOKEN1", page("www.example.com"), "", ErrNoMatch},
		{"https://tinyurl.com/TOKEN1", "https://tinyurl.com/preview/TOKEN1", page("//www.example.com"), "", ErrNoMatch},
		{"https://tinyurl.com/TOKEN1", "https://tinyurl.com/preview/TOKEN1", page(""), "", ErrNoMatch},
		{"https://tinyurl.com/TOKEN1", "https://tinyurl.com/preview/TOKEN1", `<a href="https://www.example.com">`, "", ErrNoMatch},
		{"https://tinyurl.com/", "", "", "", ErrNotShortLink},
		{"https://tinyurl.com", "", "", "", ErrNotShortLink},
	}
	for _, tt := range tests {
		pages := map[string]served{}
		if tt.fetch != "" {
			pages[tt.fetch] = served{status: 200, body: tt.body}
		}
		next, err, reqs := runStrategy(t, entry, entry.Strategy, tt.in, pages)
		if want := []string{tt.fetch}; tt.fetch == "" && len(reqs) != 0 || tt.fetch != "" && (len(reqs) != 1 || reqs[0] != want[0]) {
			t.Errorf("%s: requests %q, want %q", tt.in, reqs, tt.fetch)
		}
		if next != tt.want || !errors.Is(err, tt.err) || (err == nil) != (tt.err == nil) {
			t.Errorf("%s: %q, %v; want %q, %v", tt.in, next, err, tt.want, tt.err)
		}
	}
}

func TestBitlyInfo(t *testing.T) {
	entry := Entry{Name: "bit.ly", Match: []string{"*://bit.ly/*", "*://bitly.com/*"}, Strategy: BitlyInfo(), Kind: Marketing}
	page := `<ul><li>
                <a class="item-detail--url"
                  href="https://example.net/p?a=1&amp;b=2"
                  target="_blank">x</a></li></ul>`
	for _, in := range []string{"https://bit.ly/TOKEN1", "https://bit.ly/TOKEN1+", "http://BIT.LY/TOKEN1#x", "https://bitly.com/TOKEN1"} {
		next, err, reqs := runStrategy(t, entry, entry.Strategy, in,
			map[string]served{"https://bit.ly/TOKEN1+": {status: 200, body: page}})
		if err != nil || next != "https://example.net/p?a=1&b=2" || strings.Join(reqs, " ") != "https://bit.ly/TOKEN1+" {
			t.Errorf("%s: %q, %v, requests %q", in, next, err, reqs)
		}
	}
	next, err, reqs := runStrategy(t, entry, entry.Strategy, "https://bit.ly/TOKEN1",
		map[string]served{"https://bit.ly/TOKEN1+": {status: 200, body: `<a href="https://bitly.com/">`}})
	if !errors.Is(err, ErrNoMatch) || next != "" || len(reqs) != 1 {
		t.Errorf("no anchor: %q, %v, %q", next, err, reqs)
	}
	_, err, reqs = runStrategy(t, entry, entry.Strategy, "https://bit.ly/TOKEN1", nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 || len(reqs) != 1 {
		t.Errorf("404: %v, %q", err, reqs)
	}
	for _, in := range []string{"https://bit.ly/", "https://bit.ly/+"} {
		if _, err, reqs := runStrategy(t, entry, entry.Strategy, in, nil); !errors.Is(err, ErrNotShortLink) || len(reqs) != 0 {
			t.Errorf("%s: %v, %q", in, err, reqs)
		}
	}
}

// TestStrategiesHonourFetcher checks that the built-in strategies' fetches
// go through the Fetcher's checks: one outside the entry is ErrFetchDenied.
func TestStrategiesHonourFetcher(t *testing.T) {
	for _, tt := range []struct {
		s  Strategy
		in string
	}{
		{TikTokOEmbed(), "https://vm.tiktok.com/TOKEN1/"},
		{TinyURLPreview(), "https://vm.tiktok.com/TOKEN1"},
		{BitlyInfo(), "https://vm.tiktok.com/TOKEN1"},
	} {
		entry := Entry{Name: "e", Match: []string{"*://vm.tiktok.com/*"}, Strategy: tt.s}
		if _, err, reqs := runStrategy(t, entry, tt.s, tt.in, nil); !errors.Is(err, ErrFetchDenied) || len(reqs) != 0 {
			t.Errorf("%s: %v, %q", tt.s.Name(), err, reqs)
		}
	}
}
