package unshorten_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Espresso-Aficionados/linkclean"
	"github.com/Espresso-Aficionados/linkclean/discordlink"
	"github.com/Espresso-Aficionados/linkclean/unshorten"
)

// A *Resolver is a discordlink.Processor.
var _ discordlink.Processor = (*unshorten.Resolver)(nil)

// shortener stands in for the network in these examples: an in-process
// RoundTripper that answers each short link with a redirect and fails every
// other request. Production code leaves Config.Transport nil, which gives
// the guarded default transport, and never sets AllowUnguardedTransport for
// a transport like this one.
type shortener map[string]string // request URL → Location

func (s shortener) RoundTrip(req *http.Request) (*http.Response, error) {
	loc, ok := s[req.URL.String()]
	if !ok {
		return nil, fmt.Errorf("example: no route for %s", req.URL.Host)
	}
	return &http.Response{
		StatusCode: http.StatusMovedPermanently,
		Header:     http.Header{"Location": {loc}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// exampleConfig returns a Config whose requests go to s instead of the
// network.
func exampleConfig(s shortener) unshorten.Config {
	return unshorten.Config{Transport: s, AllowUnguardedTransport: true}
}

// One Resolver per process, and one WithCleaner view per affiliate policy:
// the views share the cache, the rate limits and the request bounds, so the
// second view's lookup of the same short link makes no request.
func ExampleNew() {
	r, err := unshorten.New(exampleConfig(shortener{
		"https://amzn.to/TOKEN1": "https://www.amazon.com/dp/B0?tag=aff-20&linkCode=ll1&ref_=as_li",
	}))
	if err != nil {
		panic(err)
	}
	strip := r.WithCleaner(linkclean.MustNew(linkclean.Config{Affiliate: linkclean.AffiliateStrip}))

	ctx := context.Background()
	for _, v := range []*unshorten.Resolver{r, strip} {
		res, err := v.Resolve(ctx, "https://amzn.to/TOKEN1")
		if err != nil {
			panic(err)
		}
		fmt.Println(res.URL, "cached:", res.Cached)
	}
	// Output:
	// https://www.amazon.com/dp/B0?tag=aff-20&linkCode=ll1 cached: false
	// https://www.amazon.com/dp/B0 cached: true
}

// Resolve cleans the input offline before any request, so the shortener
// never sees its tracking parameters, then cleans the destination it
// returns. A link that is not a registered short link makes no request and
// no error.
func ExampleResolver_Resolve() {
	r, err := unshorten.New(exampleConfig(shortener{
		"https://t.co/TOKEN1": "https://example.com/post?id=7&utm_source=twitter",
	}))
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	for _, in := range []string{"http://t.co/TOKEN1?utm_source=x", "https://example.com/?fbclid=1"} {
		res, err := r.Resolve(ctx, in)
		fmt.Println(res.URL, "resolved:", res.Resolved, "err:", err)
		for _, h := range res.Hops {
			fmt.Printf("  %s %s -> %s (%d)\n", h.Entry, h.From, h.To, h.Status)
		}
	}
	// Output:
	// https://example.com/post?id=7 resolved: true err: <nil>
	//   t.co https://t.co/TOKEN1 -> https://example.com/post?id=7&utm_source=twitter (301)
	// https://example.com/ resolved: false err: <nil>
}

// Resolution fails open: when the shortener cannot be reached, the URL is
// the offline clean of the input and the error says why.
func ExampleResolver_Resolve_failure() {
	r, err := unshorten.New(exampleConfig(shortener{})) // every request fails
	if err != nil {
		panic(err)
	}
	res, err := r.Resolve(context.Background(), "https://t.co/TOKEN1?utm_source=x")
	fmt.Println(res.URL)
	fmt.Println(errors.Is(err, unshorten.ErrUnresolved), errors.Is(err, unshorten.ErrTransient))
	// Output:
	// https://t.co/TOKEN1
	// true true
}

// Process gives a linkclean.Result, so a Resolver can be used wherever
// discordlink takes a Processor.
func ExampleResolver_Process() {
	r, err := unshorten.New(exampleConfig(shortener{
		"https://t.co/TOKEN1": "https://example.com/post?id=7&utm_source=twitter",
	}))
	if err != nil {
		panic(err)
	}
	content := "look https://t.co/TOKEN1 and https://example.com/about"
	cs, _ := discordlink.Process(context.Background(), content, r, discordlink.Options{})
	for _, c := range cs {
		fmt.Println(c.Link.URL, "->", c.Result.URL)
		for _, h := range c.Result.Hops {
			cat, _ := h.Category.MarshalText()
			fmt.Println("  hop", string(cat), h.Rule)
		}
		for _, rm := range c.Result.Removed {
			cat, _ := rm.Category.MarshalText()
			fmt.Println("  removed", string(cat), rm.Name)
		}
	}
	// Output:
	// https://t.co/TOKEN1 -> https://example.com/post?id=7
	//   hop unshortened unshorten:t.co
	//   removed tracker utm_source
}

// Match reports offline whether a URL would be resolved, and by which entry.
func ExampleResolver_Match() {
	r, err := unshorten.New(unshorten.Config{}) // New makes no request
	if err != nil {
		panic(err)
	}
	for _, in := range []string{
		"https://t.co/TOKEN1",
		"HTTP://AMZN.TO/TOKEN2",
		"https://t.co:8443/TOKEN1", // not requestable: a non-default port
		"https://bit.ly/TOKEN3",    // not registered by default
		"https://example.com/",
	} {
		entry, ok := r.Match(in)
		fmt.Printf("%s %q %v\n", in, entry, ok)
	}
	// Output:
	// https://t.co/TOKEN1 "t.co" true
	// HTTP://AMZN.TO/TOKEN2 "amazon" true
	// https://t.co:8443/TOKEN1 "" false
	// https://bit.ly/TOKEN3 "" false
	// https://example.com/ "" false
}

// bit.ly and tinyurl.com are not registered by default. Registering them
// with With makes bit.ly links resolve through bit.ly's "+" info page under
// GenericPolicy PreviewOnly (the default); under Skip they are never
// requested.
func ExampleRegistry_With() {
	reg, err := unshorten.DefaultRegistry().With(
		unshorten.Entry{
			Name:     "bit.ly",
			Match:    []string{"*://bit.ly/*", "*://bitly.com/*"},
			Strategy: unshorten.BitlyInfo(),
			Fallback: unshorten.Redirect(), // used only under Follow
			Kind:     unshorten.Marketing,
			IsFallback: func(_, t *url.URL) bool {
				h := strings.ToLower(t.Hostname())
				return (h == "bitly.com" || h == "www.bitly.com" || h == "bit.ly") &&
					(t.Path == "" || t.Path == "/") && t.RawQuery == ""
			},
		},
		unshorten.Entry{
			Name:     "tinyurl",
			Match:    []string{"*://tinyurl.com/*"},
			Strategy: unshorten.TinyURLPreview(),
			Fallback: unshorten.Redirect(), // used only under Follow
			Kind:     unshorten.Marketing,
		},
	)
	if err != nil {
		panic(err)
	}
	for _, p := range []unshorten.GenericPolicy{unshorten.PreviewOnly, unshorten.Skip} {
		r, err := unshorten.New(unshorten.Config{Registry: reg, Generic: p})
		if err != nil {
			panic(err)
		}
		bitly, ok1 := r.Match("https://bit.ly/TOKEN1")
		tiny, ok2 := r.Match("https://tinyurl.com/TOKEN2")
		fmt.Printf("%q %v %q %v\n", bitly, ok1, tiny, ok2)
	}
	// Output:
	// "bit.ly" true "tinyurl" true
	// "" false "" false
}
